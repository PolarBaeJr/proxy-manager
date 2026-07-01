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

	labelEnable  = "proxy.enable"
	labelHost    = "proxy.host"
	labelPort    = "proxy.port"
	labelPath    = "proxy.path"
	labelStrip   = "proxy.strip"
	labelName    = "proxy.name"
	labelWeight  = "proxy.weight"
	labelHealth  = "proxy.health"
	labelService = "proxy.service"
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
	Labels          map[string]string `json:"Labels"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
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

// exitInfo is the subset of /containers/{id}/json we need to distinguish a
// user-initiated stop from a crash. Docker's list endpoint doesn't return
// exit code / OOMKilled, so this is one inspect call per exited container.
type exitInfo struct {
	Code      int
	OOMKilled bool
}

// looksLikeStop returns true when the container's exit conditions match a
// clean shutdown: exit 0 (self-exit), 143 (SIGTERM handled), or 137 without
// OOMKilled (SIGKILL after `docker stop` timeout). Anything else — non-zero
// custom codes, OOM kills, segfaults — is treated as a crash.
func (e exitInfo) looksLikeStop() bool {
	if e.OOMKilled {
		return false
	}
	switch e.Code {
	case 0, 137, 143:
		return true
	default:
		return false
	}
}

func (c *dockerClient) inspectExit(ctx context.Context, id string) (exitInfo, error) {
	body, err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/json")
	if err != nil {
		return exitInfo{}, err
	}
	defer body.Close()
	var full struct {
		State struct {
			ExitCode  int  `json:"ExitCode"`
			OOMKilled bool `json:"OOMKilled"`
		} `json:"State"`
	}
	if err := json.NewDecoder(body).Decode(&full); err != nil {
		return exitInfo{}, err
	}
	return exitInfo{Code: full.State.ExitCode, OOMKilled: full.State.OOMKilled}, nil
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
