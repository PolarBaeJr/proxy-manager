package main

import (
	"crypto/x509"
	"reflect"
	"testing"
	"time"
)

func TestParseProbeTargets(t *testing.T) {
	out := parseProbeTargets("bare.example.com, host.example.com@1.2.3.4:8443 , ", "def.dial:443")
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (blank skipped)", len(out))
	}
	if out[0].SNI != "bare.example.com" || out[0].DialTarget != "def.dial:443" {
		t.Fatalf("bare = %+v", out[0])
	}
	if out[1].SNI != "host.example.com" || out[1].DialTarget != "1.2.3.4:8443" {
		t.Fatalf("split = %+v", out[1])
	}

	// Empty default falls back to host.docker.internal:443.
	def := parseProbeTargets("bare.example.com", "")
	if def[0].DialTarget != "host.docker.internal:443" {
		t.Fatalf("default dial = %q", def[0].DialTarget)
	}
}

func TestSplitTrim(t *testing.T) {
	got := splitTrim(" a , b ,, c ", ',')
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitTrim = %v, want %v", got, want)
	}
	if splitTrim("", ',') != nil {
		t.Fatal("empty input should yield nil")
	}
}

func TestFillStatus(t *testing.T) {
	cases := []struct {
		notAfter time.Time
		want     string
	}{
		{time.Now().Add(-time.Hour), "expired"},
		{time.Now().Add(3 * 24 * time.Hour), "critical"},
		{time.Now().Add(20 * 24 * time.Hour), "warning"},
		{time.Now().Add(60 * 24 * time.Hour), "ok"},
	}
	for _, c := range cases {
		info := &CertInfo{}
		fill(info, &x509.Certificate{NotAfter: c.notAfter, DNSNames: []string{"a.com", "b.com"}})
		if info.Status != c.want {
			t.Errorf("NotAfter=%v status = %q, want %q", c.notAfter, info.Status, c.want)
		}
		if !reflect.DeepEqual(info.SANs, []string{"a.com", "b.com"}) {
			t.Errorf("SANs = %v, want copied", info.SANs)
		}
	}
}

func TestWorst(t *testing.T) {
	p := &CertProber{results: map[string]*CertInfo{
		"a": {Status: "ok"},
		"b": {Status: "warning"},
	}}
	if got := p.Worst(); got != "warning" {
		t.Fatalf("Worst = %q, want warning", got)
	}
	p.results["c"] = &CertInfo{Status: "critical"}
	if got := p.Worst(); got != "critical" {
		t.Fatalf("Worst = %q, want critical", got)
	}
	p.results["d"] = &CertInfo{Status: "expired"}
	if got := p.Worst(); got != "expired" {
		t.Fatalf("Worst = %q, want expired", got)
	}
}
