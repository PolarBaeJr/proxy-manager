package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchHealthUpAndDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/up":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"up","targets":[{"name":"proxy","health":"up"}],"checked_at":"2026-01-01T00:00:00Z"}`))
		case "/degraded":
			// /api/health writes 503 for a degraded status but still writes a body.
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"degraded","targets":[{"name":"proxy","health":"down"}],"checked_at":"2026-01-01T00:00:00Z"}`))
		case "/garbage":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not json`))
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	client := srv.Client()

	hs, err := fetchHealth(context.Background(), srv.URL+"/up", client)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	if hs.Status != "up" {
		t.Errorf("up: status = %q, want up", hs.Status)
	}

	hs, err = fetchHealth(context.Background(), srv.URL+"/degraded", client)
	if err != nil {
		t.Fatalf("degraded (503): unexpected error — the body must still be read: %v", err)
	}
	if hs.Status != "degraded" || len(hs.Targets) != 1 || hs.Targets[0].Health != "down" {
		t.Errorf("degraded: got %+v", hs)
	}

	if _, err := fetchHealth(context.Background(), srv.URL+"/garbage", client); err == nil {
		t.Error("garbage body: want a decode error")
	}
	if _, err := fetchHealth(context.Background(), srv.URL+"/teapot", client); err == nil {
		t.Error("unexpected status code: want an error")
	}
	if _, err := fetchHealth(context.Background(), "http://127.0.0.1:1/nope", client); err == nil {
		t.Error("connection refused: want an error")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		hs   healthStatus
		err  error
		want string
	}{
		{"up", healthStatus{Status: "up"}, nil, "up"},
		{"degraded", healthStatus{Status: "degraded"}, nil, "degraded"},
		{"fetch error", healthStatus{}, context.DeadlineExceeded, "unreachable"},
		{"empty status no error", healthStatus{}, nil, "unreachable"},
	}
	for _, c := range cases {
		if got := classify(c.hs, c.err); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTransitionMessage(t *testing.T) {
	// No baseline yet (startup) — must never alert, even though "" != "up".
	if msg := transitionMessage("", "up", healthStatus{}, nil); msg != "" {
		t.Errorf("first poll should not alert, got %q", msg)
	}
	// No change — no alert.
	if msg := transitionMessage("up", "up", healthStatus{}, nil); msg != "" {
		t.Errorf("unchanged status should not alert, got %q", msg)
	}
	// up -> unreachable must mention the error.
	msg := transitionMessage("up", "unreachable", healthStatus{}, context.DeadlineExceeded)
	if !strings.Contains(msg, "unreachable") || !strings.Contains(msg, context.DeadlineExceeded.Error()) {
		t.Errorf("unreachable transition = %q, want it to mention the error", msg)
	}
	// unreachable -> up is the recovery message.
	msg = transitionMessage("unreachable", "up", healthStatus{}, nil)
	if !strings.Contains(strings.ToLower(msg), "healthy") {
		t.Errorf("recovery transition = %q, want a healthy/back-up message", msg)
	}
	// up -> degraded must name the down target.
	hs := healthStatus{Status: "degraded", Targets: []healthTarget{{Name: "proxy", Health: "down"}, {Name: "edge", Health: "up"}}}
	msg = transitionMessage("up", "degraded", hs, nil)
	if !strings.Contains(msg, "proxy") || strings.Contains(msg, "edge (up)") {
		t.Errorf("degraded transition = %q, want it to name the down target only", msg)
	}
}

func TestStatusReply(t *testing.T) {
	if r := statusReply(healthStatus{Status: "up", CheckedAt: "t"}, nil); !strings.Contains(r, "up") {
		t.Errorf("up reply = %q", r)
	}
	if r := statusReply(healthStatus{}, context.DeadlineExceeded); !strings.Contains(r, "unreachable") {
		t.Errorf("unreachable reply = %q", r)
	}
	hs := healthStatus{Status: "degraded", CheckedAt: "t", Targets: []healthTarget{{Name: "proxy", Health: "down"}}}
	if r := statusReply(hs, nil); !strings.Contains(r, "proxy") {
		t.Errorf("degraded reply = %q, want it to name proxy", r)
	}
}

func TestDegradedSummaryNoDetail(t *testing.T) {
	if s := degradedSummary(healthStatus{Targets: nil}); s == "" {
		t.Error("empty target list should still return a non-empty summary")
	}
}
