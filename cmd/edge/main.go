// edge: outermost network layer.
// Does the nginx-shaped jobs the proxy intentionally skips:
//   - TLS termination (Let's Encrypt via autocert, or static cert paths)
//   - HTTP → HTTPS redirect
//   - Per-IP rate limiting (optionally cluster-wide via peer gossip)
//   - Request body size cap
//   - gzip compression of text responses
//   - Real-IP / X-Forwarded-* header normalization (from RemoteAddr — we
//     never trust inbound X-Forwarded-* since we are the outermost hop)
//   - HSTS on TLS responses
//   - Access log (one JSONL line per request)
//
// Hands the request off to an internal upstream (the proxy on :8092).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

func main() {
	httpsAddr := flag.String("https-addr", ":443", "TLS listen address")
	httpAddr := flag.String("http-addr", ":80", "plain HTTP listen (ACME challenges + redirect to HTTPS)")
	backend := flag.String("backend", "http://proxy:8092", "upstream URL (the proxy)")

	tlsDomains := flag.String("tls-domains", "", "comma-separated domains for Let's Encrypt autocert (e.g. foo.example,bar.example)")
	tlsCert := flag.String("tls-cert", "", "static TLS cert path (alternative to autocert)")
	tlsKey := flag.String("tls-key", "", "static TLS key path")
	certDir := flag.String("cert-dir", "/data/certs", "autocert cache directory")

	rateLimit := flag.Int("rate", 100, "sustained requests per second per IP (0 = disabled)")
	rateBurst := flag.Int("burst", 200, "burst capacity per IP")
	maxBody := flag.Int64("max-body", 10<<20, "max request body bytes")
	insecure := flag.Bool("insecure", false, "skip TLS, serve plain HTTP on -http-addr (testing only)")
	metricsAddr := flag.String("metrics-addr", ":8094", "internal metrics endpoint listen address")

	peers := flag.String("peers", "", "comma-separated peer edge base URLs for rate-limit gossip (e.g. http://edge-eu.tailnet:8094)")
	peerSecret := flag.String("peer-secret", os.Getenv("EDGE_PEER_SECRET"), "shared bearer for peer gossip (also read from EDGE_PEER_SECRET)")
	peerInterval := flag.Duration("peer-interval", 2*time.Second, "how often to push consumption deltas to peers")

	readTimeout := flag.Duration("read-timeout", 30*time.Second, "server ReadTimeout — full request body must arrive within this window")
	writeTimeout := flag.Duration("write-timeout", 60*time.Second, "server WriteTimeout — response must complete within this window")
	idleTimeout := flag.Duration("idle-timeout", 120*time.Second, "server IdleTimeout — keep-alive idle cutoff")
	flag.Parse()

	metrics := NewMetrics()
	rl := newRateLimiter(*rateLimit, *rateBurst)

	// Peer gossip: only enabled when a shared secret is present.
	peerList := splitAndTrim(*peers)
	var gossip http.Handler
	if *peerSecret != "" {
		gossip = gossipHandler(rl, *peerSecret)
		if len(peerList) > 0 {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			ps := newPeerSync(rl, peerList, *peerSecret, *peerInterval)
			go ps.Run(ctx)
			log.Printf("edge peers: gossiping to %d peer(s) every %s", len(peerList), *peerInterval)
		} else {
			log.Printf("edge peers: /gossip enabled (receive-only, no outbound peers configured)")
		}
	} else if len(peerList) > 0 {
		log.Printf("edge peers: peers configured but -peer-secret / EDGE_PEER_SECRET empty — gossip disabled")
	}

	metricsServer(*metricsAddr, metrics, gossip)
	log.Printf("metrics on %s/metrics", *metricsAddr)

	// Build the request handler chain. Outer-most first.
	var handler http.Handler = newForwarder(*backend)
	handler = withMaxBody(handler, *maxBody)
	handler = withGzip(handler)
	if *rateLimit > 0 {
		handler = withRateLimit(handler, rl)
	}
	handler = withForwardedHeaders(handler)
	handler = withHSTS(handler)
	handler = withMetrics(handler, metrics)
	handler = withAccessLog(handler)

	newServer := func(addr string, h http.Handler, tlsCfg *tls.Config) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           h,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       *readTimeout,
			WriteTimeout:      *writeTimeout,
			IdleTimeout:       *idleTimeout,
		}
	}

	// ----- Insecure mode (HTTP only) — for local testing, not production. -----
	if *insecure {
		log.Printf("edge: insecure HTTP on %s → %s", *httpAddr, *backend)
		srv := newServer(*httpAddr, handler, nil)
		log.Fatal(srv.ListenAndServe())
		return
	}

	// ----- Static cert mode. -----
	if *tlsCert != "" && *tlsKey != "" {
		log.Printf("edge: HTTPS on %s (static cert) → %s", *httpsAddr, *backend)
		go redirectHTTPSServer(*httpAddr, nil, *readTimeout, *writeTimeout, *idleTimeout)
		srv := newServer(*httpsAddr, handler, &tls.Config{MinVersion: tls.VersionTLS12})
		if err := srv.ListenAndServeTLS(*tlsCert, *tlsKey); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	}

	// ----- Autocert (Let's Encrypt) mode. -----
	domains := splitAndTrim(*tlsDomains)
	if len(domains) == 0 {
		log.Fatal("no TLS mode chosen — pass -tls-domains for autocert, -tls-cert/-tls-key for static, or -insecure for HTTP-only")
	}
	if err := os.MkdirAll(*certDir, 0o700); err != nil {
		log.Fatalf("cert dir: %v", err)
	}
	m := &autocert.Manager{
		Cache:      autocert.DirCache(*certDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domains...),
	}
	go redirectHTTPSServer(*httpAddr, m, *readTimeout, *writeTimeout, *idleTimeout)
	srv := newServer(*httpsAddr, handler, &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12})
	log.Printf("edge: HTTPS on %s for %v → %s", *httpsAddr, domains, *backend)
	if err := srv.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// redirectHTTPSServer listens on plain HTTP, redirecting everything to HTTPS.
// When `m` is set, ACME HTTP-01 challenges are served instead of redirecting.
func redirectHTTPSServer(addr string, m *autocert.Manager, read, write, idle time.Duration) {
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	if m != nil {
		h = m.HTTPHandler(h)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       read,
		WriteTimeout:      write,
		IdleTimeout:       idle,
	}
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP listener (%s): %v", addr, err)
	}
}

// newForwarder is a single-host reverse proxy to the internal upstream.
// All requests funnel here after the middleware chain.
func newForwarder(backend string) http.Handler {
	u, err := url.Parse(backend)
	if err != nil {
		log.Fatalf("bad -backend: %v", err)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	orig := p.Director
	p.Director = func(r *http.Request) {
		orig(r)
		// Preserve the original Host so the upstream's host-based routing works.
		r.Header.Set("X-Forwarded-Host", r.Host)
		// The Director rewrites r.URL.Host to the backend; the proxy reads r.Host
		// (not URL.Host) for routing, so leave r.Host alone.
	}
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("upstream %s: %v", backend, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
	return p
}

// withMaxBody wraps the handler with a hard request-body size cap.
func withMaxBody(h http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		h.ServeHTTP(w, r)
	})
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
