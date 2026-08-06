// mcp — an MCP server exposing the dashboard's operations as tools.
//
// Mounted under a path on a shared MCP host (mcp.<domain>/mcp/dashboard) and
// fronted by the proxy with proxy.auth.mode=oauth, which binds a token to that
// exact host+path. Authentication is therefore entirely the proxy's job; this
// binary trusts whatever reaches it and must never be published directly.
//
// Writes are OFF by default. With MCP_ALLOW_WRITES unset only read tools are
// registered, so a model can inspect the homelab but not change it.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
)

var version = "dev"

func main() {
	addr := flag.String("addr", ":8097", "listen address")
	dashURL := flag.String("dashboard-url", envOr("DASHBOARD_URL", "http://dashboard:8093"), "dashboard base URL")
	flag.Parse()

	token := strings.TrimSpace(os.Getenv("DASHBOARD_TOKEN"))
	if token == "" {
		// Fail loudly at startup rather than serving a tool list whose every
		// call 401s — a model would read that as "the homelab is broken".
		log.Fatal("DASHBOARD_TOKEN is required: create an API token in the dashboard (Settings → tokens) and set it here")
	}

	allowWrites := isTrue(os.Getenv("MCP_ALLOW_WRITES"))

	srv := NewServer("proxy-manager-dashboard", version)
	registerTools(srv, newDashClient(*dashURL, token), allowWrites)

	mux := http.NewServeMux()
	// Mounted at "/" because the proxy strips the /mcp/dashboard prefix before
	// forwarding. Registering the prefix here too would double it.
	mux.Handle("/", srv)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mode := "read-only"
	if allowWrites {
		mode = "READ-WRITE (MCP_ALLOW_WRITES set)"
	}
	log.Printf("mcp on %s — %d tools, %s, dashboard=%s", *addr, len(srv.tools), mode, *dashURL)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
