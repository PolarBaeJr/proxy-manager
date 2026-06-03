// edge: outermost network layer.
// Does the nginx-shaped jobs the proxy intentionally skips:
//   - TLS termination (Let's Encrypt via autocert, or static cert paths)
//   - HTTP → HTTPS redirect
//   - Per-IP rate limiting
//   - Request body size cap
//   - gzip compression of text responses
//   - Real-IP / X-Forwarded-* header normalization
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
	"strings"
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
	flag.Parse()

	metrics := NewMetrics()
	metricsServer(*metricsAddr, metrics)
	log.Printf("metrics on %s/metrics", *metricsAddr)

	// Build the request handler chain. Outer-most first.
	var handler http.Handler = newForwarder(*backend)
	handler = withMaxBody(handler, *maxBody)
	handler = withGzip(handler)
	if *rateLimit > 0 {
		handler = withRateLimit(handler, *rateLimit, *rateBurst)
	}
	handler = withForwardedHeaders(handler)
	handler = withMetrics(handler, metrics)
	handler = withAccessLog(handler)

	// ----- Insecure mode (HTTP only) — for local testing, not production. -----
	if *insecure {
		log.Printf("edge: insecure HTTP on %s → %s", *httpAddr, *backend)
		log.Fatal(http.ListenAndServe(*httpAddr, handler))
		return
	}

	// ----- Static cert mode. -----
	if *tlsCert != "" && *tlsKey != "" {
		log.Printf("edge: HTTPS on %s (static cert) → %s", *httpsAddr, *backend)
		go redirectHTTPSServer(*httpAddr, nil)
		srv := &http.Server{
			Addr:              *httpsAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
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
	go redirectHTTPSServer(*httpAddr, m)
	srv := &http.Server{
		Addr:              *httpsAddr,
		Handler:           handler,
		TLSConfig:         &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("edge: HTTPS on %s for %v → %s", *httpsAddr, domains, *backend)
	if err := srv.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// redirectHTTPSServer listens on plain HTTP, redirecting everything to HTTPS.
// When `m` is set, ACME HTTP-01 challenges are served instead of redirecting.
func redirectHTTPSServer(addr string, m *autocert.Manager) {
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	if m != nil {
		h = m.HTTPHandler(h)
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
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

// Used by handlers that need a ctx without an inbound request.
var _ = context.Background
