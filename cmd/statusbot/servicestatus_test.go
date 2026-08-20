package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchServiceStatusSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer pmt_abc" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer pmt_abc")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"sampled_at": "2026-08-19T16:40:00Z",
			"stats_sampled_at": "2026-08-19T16:39:55Z",
			"window_seconds": 300,
			"groups": [
				{"group": "badminton", "services": [
					{"name": "player", "routed": true, "host": "sfubadminton.com",
					 "healthy_replicas": 2, "total_replicas": 2, "state": "up",
					 "requests_5m": 134, "rate_truncated": false,
					 "cpu_pct": 3.2, "mem_used_bytes": 123456789, "mem_limit_bytes": 536870912},
					{"name": "badminton-db", "routed": false, "requests_5m": null,
					 "cpu_pct": 1.1, "mem_used_bytes": 98765432, "mem_limit_bytes": 268435456}
				]}
			]
		}`))
	}))
	defer srv.Close()

	resp, err := fetchServiceStatus(context.Background(), srv.URL, "pmt_abc", srv.Client())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].Group != "badminton" {
		t.Fatalf("Groups = %+v", resp.Groups)
	}
	svcs := resp.Groups[0].Services
	if len(svcs) != 2 {
		t.Fatalf("Services = %+v, want 2", svcs)
	}
	if svcs[0].Name != "player" || svcs[0].Requests5m == nil || *svcs[0].Requests5m != 134 {
		t.Errorf("svcs[0] = %+v", svcs[0])
	}
	if svcs[1].Name != "badminton-db" || svcs[1].Requests5m != nil {
		t.Errorf("svcs[1] = %+v, want Requests5m nil", svcs[1])
	}
}

func TestFetchServiceStatusMissingToken(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := fetchServiceStatus(context.Background(), srv.URL, "", srv.Client())
	if err == nil {
		t.Fatal("want an error when the token is empty")
	}
	if !errors.Is(err, errNoDashboardToken) {
		t.Errorf("err = %v, want errNoDashboardToken", err)
	}
	if called {
		t.Error("fetchServiceStatus made a request with no token — want it to no-op instead")
	}
}

func TestFetchServiceStatusNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := fetchServiceStatus(context.Background(), srv.URL, "pmt_abc", srv.Client())
	if err == nil {
		t.Fatal("want an error for a non-200 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want it to mention the status code", err)
	}
}

func TestFetchServiceStatusUnreachable(t *testing.T) {
	_, err := fetchServiceStatus(context.Background(), "http://127.0.0.1:1/nope", "pmt_abc", http.DefaultClient)
	if err == nil {
		t.Fatal("connection refused: want an error")
	}
}

func TestFetchServiceStatusGarbageBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	_, err := fetchServiceStatus(context.Background(), srv.URL, "pmt_abc", srv.Client())
	if err == nil {
		t.Fatal("garbage body: want a decode error")
	}
}
