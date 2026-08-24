package main

import (
	"net/http"
	"time"
)

// peerServer exposes /peer/handshake on a dedicated listener, separate from
// both the public dashboard port (:8093) and the internal metrics port
// (:8094). A dedicated port — rather than reusing the metrics port, as
// cmd/proxy does — keeps /metrics (hostnames, request counts) from being
// exposed unauthenticated across the Tailscale mesh: nginx's TCP-level
// stream proxy used for cross-host transport can only gate by port, not by
// path. Only started when DASHBOARD_PEER_SECRET is set (see main.go).
func peerServer(addr string, handshake http.Handler) {
	mux := http.NewServeMux()
	mux.Handle("/peer/handshake", handshake)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
}
