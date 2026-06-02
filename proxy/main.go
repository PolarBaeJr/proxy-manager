// proxy: request-path only. Reverse proxy + load balancer + health checks.
// Read-only access to the Docker socket. No auth, no management endpoints.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", ":8092", "proxy listen address")
	staticConfig := flag.String("config", "/etc/proxy/routes.json", "static routes JSON (ignored if missing)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dc := newDockerClient()
	router := &Router{}
	refresh := func() {
		groups, err := assembleGroups(ctx, dc, *staticConfig)
		if err != nil {
			log.Printf("refresh: %v", err)
			return
		}
		router.Set(groups)
		total := 0
		for _, g := range groups {
			total += len(g.Backends)
		}
		log.Printf("loaded %d route(s), %d backend(s)", len(groups), total)
	}
	refresh()

	go dc.streamEvents(ctx, func(action string) {
		switch action {
		case "start", "die", "destroy", "kill", "stop":
			refresh()
		}
	})
	go runHealthChecks(ctx, router)

	log.Printf("proxy on %s", *addr)
	if err := http.ListenAndServe(*addr, router); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
