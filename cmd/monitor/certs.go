package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"strings"
	"sync"
	"time"
)

// CertProber periodically TLS-handshakes a list of hostnames and records the
// peer certificate's issuer + expiry. Works against any TLS-terminating
// upstream — nginx, edge (when running), even Cloudflare's public edge.
//
// We deliberately set InsecureSkipVerify because:
//   - probing your own origin cert (self-signed) should still report metadata
//   - chain validation isn't the point; expiry tracking is

type CertInfo struct {
	Host        string    `json:"host"`         // SNI we asked for
	DialTarget  string    `json:"dial_target"`  // actual host:port we connected to
	Issuer      string    `json:"issuer"`
	Subject     string    `json:"subject"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	DaysLeft    int       `json:"days_left"`
	Status      string    `json:"status"` // ok | warning | critical | expired | error
	SANs        []string  `json:"sans,omitempty"`
	Error       string    `json:"error,omitempty"`
	LastChecked time.Time `json:"last_checked"`
}

// ProbeTarget — what to handshake. DialTarget is the host:port to connect to;
// SNI controls the ServerName sent in the ClientHello.
type ProbeTarget struct {
	SNI        string
	DialTarget string
}

type CertProber struct {
	mu       sync.RWMutex
	targets  []ProbeTarget
	results  map[string]*CertInfo // keyed by SNI
	interval time.Duration
}

func NewCertProber(targets []ProbeTarget, interval time.Duration) *CertProber {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	return &CertProber{
		targets:  targets,
		results:  map[string]*CertInfo{},
		interval: interval,
	}
}

func (p *CertProber) Run(ctx context.Context) {
	p.probeAll()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.probeAll()
		}
	}
}

func (p *CertProber) probeAll() {
	for _, t := range p.targets {
		info := probeOne(t)
		p.mu.Lock()
		p.results[t.SNI] = info
		p.mu.Unlock()
	}
}

func probeOne(t ProbeTarget) *CertInfo {
	info := &CertInfo{
		Host:        t.SNI,
		DialTarget:  t.DialTarget,
		LastChecked: time.Now(),
	}
	dialer := &tls.Dialer{
		NetDialer: nil, // default 0 timeout — we wrap with context below
		Config: &tls.Config{
			ServerName:         t.SNI,
			InsecureSkipVerify: true, // we want metadata; chain validation isn't the point
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", t.DialTarget)
	if err != nil {
		info.Status = "error"
		info.Error = err.Error()
		return info
	}
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		info.Status = "error"
		info.Error = "not a TLS connection"
		return info
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		info.Status = "error"
		info.Error = "no peer certificates"
		return info
	}
	leaf := state.PeerCertificates[0]
	fill(info, leaf)
	return info
}

func fill(info *CertInfo, c *x509.Certificate) {
	info.Issuer = c.Issuer.String()
	info.Subject = c.Subject.String()
	info.NotBefore = c.NotBefore
	info.NotAfter = c.NotAfter
	daysLeft := int(time.Until(c.NotAfter).Hours() / 24)
	info.DaysLeft = daysLeft
	switch {
	case time.Now().After(c.NotAfter):
		info.Status = "expired"
	case daysLeft < 7:
		info.Status = "critical"
	case daysLeft < 30:
		info.Status = "warning"
	default:
		info.Status = "ok"
	}
	info.SANs = append([]string(nil), c.DNSNames...)
}

func (p *CertProber) Snapshot() []*CertInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*CertInfo, 0, len(p.results))
	for _, v := range p.results {
		copy := *v
		out = append(out, &copy)
	}
	// stable order by SNI for the UI
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Host < out[i].Host {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (p *CertProber) Worst() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rank := map[string]int{"ok": 0, "warning": 1, "critical": 2, "expired": 3, "error": 3}
	worst := "ok"
	worstR := 0
	for _, v := range p.results {
		if r, ok := rank[v.Status]; ok && r > worstR {
			worstR = r
			worst = v.Status
		}
	}
	return worst
}

// parseProbeTargets accepts comma-separated entries. Each entry is either:
//   - "host.example.com" → probe host.docker.internal:443 with SNI=host.example.com
//   - "host.example.com@1.2.3.4:8443" → probe 1.2.3.4:8443 with SNI=host.example.com
func parseProbeTargets(s string, defaultDial string) []ProbeTarget {
	if defaultDial == "" {
		defaultDial = "host.docker.internal:443"
	}
	var out []ProbeTarget
	for _, p := range splitTrim(s, ',') {
		sni := p
		dial := defaultDial
		if at := strings.IndexByte(p, '@'); at > 0 {
			sni = p[:at]
			dial = p[at+1:]
		}
		out = append(out, ProbeTarget{SNI: sni, DialTarget: dial})
	}
	if len(out) > 0 {
		log.Printf("cert prober: %d target(s) (default dial: %s)", len(out), defaultDial)
	}
	return out
}

func splitTrim(s string, sep byte) []string {
	var out []string
	for _, seg := range strings.Split(s, string(sep)) {
		if seg = strings.TrimSpace(seg); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}
