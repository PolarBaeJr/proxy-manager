package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	dockerSock = "/var/run/docker.sock"
	dockerAPI  = "v1.43"
	// Network all label-discovered containers join so the proxy can reach them.
	managedNetwork = "edge"

	labelEnable    = "proxy.enable"
	labelHost      = "proxy.host"
	labelPort      = "proxy.port"
	labelPath      = "proxy.path"
	labelStrip     = "proxy.strip"
	labelName      = "proxy.name"
	labelWeight    = "proxy.weight"
	labelHealth    = "proxy.health"
	labelService   = "proxy.service"
	labelAuth      = "proxy.auth"
	labelAuthUsers = "proxy.auth.users"
	labelAuthMode  = "proxy.auth.mode"
	labelRateLimit = "proxy.ratelimit"
	labelRateRPM   = "proxy.ratelimit.rpm"
	// labelDropHeaders is a comma-separated list of header names to strip
	// from the request before forwarding — see RouteGroup.DropHeaders.
	labelDropHeaders = "proxy.drop_headers"
	// labelSpread opts a route out of pickHealthy's default peer-as-failover
	// tiering and into active load balancing across the mesh — see
	// RouteGroup.Spread. Set by cmd/dashboard's cross-host scale
	// (spread.go) on the replicas it places on a peer; nothing sets it
	// implicitly, so every existing route keeps failover-only semantics.
	labelSpread = "proxy.spread"
	// labelSticky opts a route into cookie-based session affinity — see
	// RouteGroup.Sticky. Boolean convention matches labelSpread: "true" merges
	// the flag on for the whole group if ANY replica carries it.
	labelSticky = "proxy.sticky"
	// labelCache opts a route into the anonymous GET/HEAD micro-cache — see
	// RouteGroup.CacheTTL. A Go duration; absent, "", "0" or "false" = off.
	// Unlike the boolean labels above, the largest non-zero value across
	// replicas wins rather than any-replica-true.
	labelCache = "proxy.cache"
	// labelCachePaths narrows labelCache to a comma-separated list of
	// client-path prefixes — see RouteGroup.CachePaths. Unioned across
	// replicas.
	labelCachePaths = "proxy.cache.paths"
	// labelCanary does not otherwise exist in the proxy package — canary is
	// managed by the dashboard (cmd/dashboard/docker.go's labelCanary) and
	// the proxy has never needed to know about it, since a canary container
	// still just carries the SAME proxy.* labels as its live siblings and
	// joins their default label-managed group like any other replica. It's
	// added here only to exclude canary containers from
	// assembleGroups' backendsByService (routes.json Service-field
	// resolution) — a DELIBERATE asymmetry with the dashboard's own
	// serviceBackends (cmd/dashboard/docker.go), which DOES include canary.
	// The motivating use case for Service-field routes.json entries is
	// per-path rate limits on sensitive paths (e.g. auth/login) on a single
	// container serving multiple internal paths — silently routing some of
	// that sensitive traffic to an in-progress canary is worse than the gap
	// of excluding it; the canary still serves everything reachable through
	// its own default label-managed route exactly as before.
	labelCanary = "proxy.canary"
)

// dockerClient is the proxy's READ-ONLY view of the Docker daemon.
// Mounted /var/run/docker.sock:ro in compose — even if the binary were
// compromised, write operations against the daemon would fail.
type dockerClient struct{ http *http.Client }

func newDockerClient() *dockerClient {
	return &dockerClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", dockerSock)
				},
			},
		},
	}
}

func (c *dockerClient) get(ctx context.Context, path string) (io.ReadCloser, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://docker/"+dockerAPI+path, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("docker GET %s: %d %s", path, resp.StatusCode, string(b))
	}
	return resp.Body, nil
}

type dockerContainer struct {
	ID              string            `json:"Id"`
	Names           []string          `json:"Names"`
	State           string            `json:"State"`
	Status          string            `json:"Status"` // raw docker status, e.g. "Up 2 minutes (healthy)"
	Labels          map[string]string `json:"Labels"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// dockerUnhealthy reports whether the container's raw Status string (from
// the /containers/json list endpoint) indicates a failing Docker
// HEALTHCHECK. Mirrors cmd/dashboard/docker.go's parseHealth string-match —
// the list endpoint has no structured State.Health.Status field, only this
// human-readable string, and calling /inspect per container per refresh is
// the cost this avoids. No HEALTHCHECK, or "(health: starting)", returns
// false — this is a floor, not a full signal.
func dockerUnhealthy(status string) bool {
	return strings.Contains(status, "(unhealthy)")
}

func (c *dockerContainer) name() string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return "?"
}

func (c *dockerClient) listEnabledContainers(ctx context.Context) ([]dockerContainer, error) {
	// all=true so stopped containers still surface — the router needs to know
	// a host *would* be served by something currently down, so it can return
	// 503 (service unavailable) instead of 404 (no such route).
	filt := url.QueryEscape(fmt.Sprintf(`{"label":["%s=true"]}`, labelEnable))
	body, err := c.get(ctx, "/containers/json?all=true&filters="+filt)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []dockerContainer
	return out, json.NewDecoder(body).Decode(&out)
}

type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
}

func (c *dockerClient) streamEvents(ctx context.Context, onAction func(string)) {
	for {
		body, err := c.get(ctx, `/events?filters={"type":["container"]}`)
		if err != nil {
			log.Printf("event stream open: %v — retry in 2s", err)
			time.Sleep(2 * time.Second)
			continue
		}
		dec := json.NewDecoder(body)
		for {
			var ev dockerEvent
			if err := dec.Decode(&ev); err != nil {
				body.Close()
				log.Printf("event stream: %v — reconnecting", err)
				break
			}
			onAction(ev.Action)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}
