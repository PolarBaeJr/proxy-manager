// Duplicate a label-managed service onto a peer host: read the service's
// live env/mounts/labels from Docker, ship them to a peer dashboard over the
// write-mesh, and have the peer create an equivalent container there. Unlike
// every other /api/services/{name}/... mutation, this one is NOT routed
// through the generic ?host=-based forwardServiceMutation (api.go) — that
// forwarder relays the raw incoming request body verbatim, but duplicating a
// service needs this host to read the REAL container's live config
// server-side (which may include secrets in Env) rather than trust whatever
// the caller claims. runServiceDuplicate calls peerMutate directly instead.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

// These mirror cmd/proxy/docker.go's label constants exactly (name and
// value) — cmd/proxy and cmd/dashboard are separate binaries, so there's no
// way to share the const block, and cmd/dashboard doesn't otherwise need
// them (labelHealth already exists here for a different feature and is
// reused, not redefined).
const (
	labelAuth      = "proxy.auth"
	labelAuthUsers = "proxy.auth.users"
	labelAuthMode  = "proxy.auth.mode"
	labelRateLimit = "proxy.ratelimit"
	labelRateRPM   = "proxy.ratelimit.rpm"
)

// DuplicateServiceRequest is the browser-facing request body for
// POST /api/services/{name}/duplicate.
type DuplicateServiceRequest struct {
	Target      string `json:"target"`
	PublishPort int    `json:"publish_port,omitempty"`
}

// DuplicateServiceResponse is the browser-facing response.
type DuplicateServiceResponse struct {
	Status          string `json:"status"`
	Target          string `json:"target"`
	Port            int    `json:"port"`
	NginxStreamHint string `json:"nginx_stream_hint,omitempty"`
}

// peerDuplicateRequest is what this host sends to the target peer's
// POST /peer/duplicate. Built entirely server-side from this host's own live
// Docker state (runServiceDuplicate) — never a passthrough of the browser's
// request body.
type peerDuplicateRequest struct {
	Name         string      `json:"name"`
	Image        string      `json:"image"`
	Env          []string    `json:"env,omitempty"`
	Host         string      `json:"host,omitempty"`
	Port         int         `json:"port"` // container-internal listen port (becomes proxy.port label)
	Path         string      `json:"path,omitempty"`
	Strip        bool        `json:"strip,omitempty"`
	Service      string      `json:"service,omitempty"`
	Unscalable   bool        `json:"unscalable,omitempty"`
	Health       string      `json:"health,omitempty"`
	PublishPort  int         `json:"publish_port,omitempty"` // host-side port published via Docker PortBindings
	Volumes      []string    `json:"volumes,omitempty"`
	VolumeMounts []mountSpec `json:"volume_mounts,omitempty"`
	Auth         bool        `json:"auth,omitempty"`
	AuthUsers    []string    `json:"auth_users,omitempty"`
	AuthMode     string      `json:"auth_mode,omitempty"`
	RateLimit    bool        `json:"ratelimit,omitempty"`
	RateRPM      int         `json:"ratelimit_rpm,omitempty"`
}

type peerDuplicateResponse struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	Port   int    `json:"port"`
}

// errDuplicateNotFound is returned by runServiceDuplicate when the named
// service has no containers at all — the API layer maps it to 404.
var errDuplicateNotFound = errors.New("service not found")

// peerDuplicateError wraps a peer's non-2xx response (or a transport-level
// failure, statusCode 0) to runServiceDuplicate's POST /peer/duplicate call
// so the API layer can map it exactly the way mapPeerMutationErr would,
// without runServiceDuplicate needing an http.ResponseWriter of its own.
type peerDuplicateError struct {
	statusCode int
	body       []byte
}

func (e *peerDuplicateError) Error() string {
	return fmt.Sprintf("peer rejected duplicate (status %d): %s", e.statusCode, string(e.body))
}

// runServiceDuplicate clones a label-managed service's live image/env/mounts/
// labels onto a peer host: pick a template replica, refuse any bind mount
// (the target host has no way to satisfy it), ship the rest to the peer's
// /peer/duplicate, and on success extend this host's own routes.json so
// traffic for the service's host+path also reaches the new peer backend.
//
// Note: because the container created on the peer publishes a host port
// (PublishPort), inspectHostConfigUnknowns will refuse to ever replace/
// onboard/autoupdate it there afterward (PortBindings is in
// hostConfigRefuseFields) — pre-existing, intentional behavior, not a bug.
func runServiceDuplicate(ctx context.Context, dc *dockerClient, registry *PeerRegistry, onb *OnboardedStore, routesConfigPath string, name string, req DuplicateServiceRequest, actorAssertion string) (DuplicateServiceResponse, error) {
	if infraContainerNames[name] {
		return DuplicateServiceResponse{}, fmt.Errorf("%q is a fixed infrastructure container", name)
	}
	svc, ok, err := findService(ctx, dc, name)
	if err != nil {
		return DuplicateServiceResponse{}, err
	}
	if !ok {
		return DuplicateServiceResponse{}, errDuplicateNotFound
	}
	// svc.Onboarded is never set by findService/listServices — only
	// buildManagedServices sets it — so it's checked here too, against the
	// actual OnboardedStore, or this guard would never fire.
	if svc.Onboarded {
		return DuplicateServiceResponse{}, fmt.Errorf("%q is an onboarded service — duplicate only supports label-managed services", name)
	}
	if onb != nil {
		if _, ok := onb.Get(name); ok {
			return DuplicateServiceResponse{}, fmt.Errorf("%q is an onboarded service — duplicate only supports label-managed services", name)
		}
	}
	if registry == nil {
		return DuplicateServiceResponse{}, fmt.Errorf("peer mesh not configured")
	}
	peerURL, ok := registry.URLForIdentity(req.Target)
	if !ok {
		return DuplicateServiceResponse{}, fmt.Errorf("unknown target host %q", req.Target)
	}
	secret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	if secret == "" {
		return DuplicateServiceResponse{}, fmt.Errorf("DASHBOARD_PEER_SECRET not configured")
	}

	all, err := dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return DuplicateServiceResponse{}, err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return DuplicateServiceResponse{}, fmt.Errorf("service %q has no live replicas", name)
	}
	tpl := existing[0]

	env, err := dc.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return DuplicateServiceResponse{}, fmt.Errorf("inspect template env: %w", err)
	}
	clone, err := dc.inspectCloneSpec(ctx, tpl.ID)
	if err != nil {
		return DuplicateServiceResponse{}, fmt.Errorf("inspect template clone spec: %w", err)
	}

	// Named volumes are carried over by NAME only — the peer's
	// peerDuplicateHandler calls createVolume for each, which creates a
	// fresh, empty volume (Docker has no server-to-server volume-data copy
	// over this API), not a copy of this host's data. Bind mounts are
	// refused outright below since there's no such thing as an "empty" bind
	// source to fall back to.
	var volumes []string
	var volumeMounts []mountSpec
	for _, m := range clone.Mounts {
		if m.Type == "bind" {
			return DuplicateServiceResponse{}, fmt.Errorf("refusing to duplicate %q: bind mount %s would not exist on the target host — resolve manually first", name, m.Source)
		}
		if m.Type == "volume" {
			volumes = append(volumes, m.Source)
		}
		volumeMounts = append(volumeMounts, m)
	}

	port := req.PublishPort
	if port == 0 {
		port = svc.Port
	}
	if !validPort(port) {
		return DuplicateServiceResponse{}, fmt.Errorf("invalid publish port")
	}

	var authUsers []string
	if raw := tpl.Labels[labelAuthUsers]; raw != "" {
		for _, u := range strings.Split(raw, ",") {
			authUsers = append(authUsers, strings.TrimSpace(u))
		}
	}
	rateRPM, _ := strconv.Atoi(tpl.Labels[labelRateRPM])

	peerReq := peerDuplicateRequest{
		Name:         name,
		Image:        tpl.Image,
		Env:          env,
		Host:         svc.Host,
		Port:         svc.Port,
		Path:         svc.Path,
		Strip:        tpl.Labels[labelStrip] == "true",
		Service:      svc.Name,
		Unscalable:   svc.Unscalable,
		Health:       tpl.Labels[labelHealth],
		PublishPort:  port,
		Volumes:      volumes,
		VolumeMounts: volumeMounts,
		Auth:         tpl.Labels[labelAuth] == "true",
		AuthUsers:    authUsers,
		AuthMode:     tpl.Labels[labelAuthMode],
		RateLimit:    tpl.Labels[labelRateLimit] == "true",
		RateRPM:      rateRPM,
	}
	reqBody, err := json.Marshal(peerReq)
	if err != nil {
		return DuplicateServiceResponse{}, err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var peerResp peerDuplicateResponse
	statusCode, respBody, mutErr := peerMutate(ctx, client, peerURL, secret, http.MethodPost, "/peer/duplicate",
		30*time.Second, bytes.NewReader(reqBody), &peerResp, actorAssertion)
	if mutErr != nil {
		return DuplicateServiceResponse{}, &peerDuplicateError{statusCode: 0, body: []byte(mutErr.Error())}
	}
	if statusCode < 200 || statusCode >= 300 {
		return DuplicateServiceResponse{}, &peerDuplicateError{statusCode: statusCode, body: respBody}
	}

	// Container confirmed up on the peer — now wire this host's own routing
	// to it. u.Hostname() is the peer's own reachable address (the same host
	// its /peer/duplicate answered from), matching PublishPort above.
	u, err := url.Parse(peerURL)
	if err != nil {
		return DuplicateServiceResponse{}, fmt.Errorf("container created on peer but failed to parse peer URL for routing: %w", err)
	}
	backend := fmt.Sprintf("http://%s:%d", u.Hostname(), port)
	if err := upsertDuplicateRoute(routesConfigPath, name, svc.Host, svc.Path, peerReq.Strip, svc.Name, backend,
		peerReq.Health, peerReq.Auth, peerReq.AuthUsers, peerReq.AuthMode, peerReq.RateLimit, peerReq.RateRPM); err != nil {
		return DuplicateServiceResponse{}, fmt.Errorf("container created on peer but failed to add route: %w", err)
	}
	proxyRefresh(proxyURLFromEnv())

	hint := fmt.Sprintf(
		"# Optional: raw TCP passthrough for %s on this host's Tailscale IP.\n"+
			"# Add to the stream{} block — Pi: /etc/nginx-stream/nginx.conf, Mac: ~/deploy/nginx-stream/nginx.conf\n"+
			"server {\n    listen <tailscale-ip>:%d;\n    proxy_pass 127.0.0.1:%d;\n}\n",
		name, port, port,
	)
	return DuplicateServiceResponse{Status: "duplicated", Target: req.Target, Port: port, NginxStreamHint: hint}, nil
}

// upsertDuplicateRoute rewrites the routes.json entry tagged
// duplicate_of=<name>, appending backend if the entry already exists rather
// than overwriting — a service can be duplicated to more than one peer.
// Mirrors upsertOnboardedRoute's read-modify-write shape exactly (including
// its lack of any locking/atomicity — writeRoutesFile is a plain
// os.WriteFile there too).
func upsertDuplicateRoute(path, name, host, routePath string, strip bool, service, backend, health string, auth bool, authUsers []string, authMode string, rateLimit bool, rateRPM int) error {
	f, err := readRoutesFile(path)
	if err != nil {
		return err
	}
	for i, r := range f.Routes {
		if r.DuplicateOf == name {
			f.Routes[i].Backends = append(f.Routes[i].Backends, backend)
			return writeRoutesFile(path, f)
		}
	}
	entry := routesEntry{
		Name:        "duplicate: " + name,
		Host:        host,
		Path:        routePath,
		Strip:       strip,
		Backends:    []string{backend},
		Service:     service,
		Health:      health,
		Auth:        auth,
		AuthUsers:   authUsers,
		AuthMode:    authMode,
		RateLimit:   rateLimit,
		RateRPM:     rateRPM,
		DuplicateOf: name,
	}
	f.Routes = append(f.Routes, entry)
	return writeRoutesFile(path, f)
}

// peerDuplicateHandler returns the HTTP handler for POST /peer/duplicate on
// the dedicated peer-handshake port — same bearer-auth/writesEnabled-gate
// shape as every other write-mesh handler in peers.go. Builds docker labels
// SERVER-SIDE from typed request fields only (never a raw label map from the
// wire), so there's no way for a caller to smuggle an arbitrary label.
func peerDuplicateHandler(secret, identity string, dc *dockerClient, writesEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || !writesEnabled {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req peerDuplicateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if !validServiceName(req.Name) {
			http.Error(w, "invalid name (allowed: a-z A-Z 0-9 . _ -, max 63 chars)", http.StatusBadRequest)
			return
		}
		if infraContainerNames[req.Name] {
			http.Error(w, fmt.Sprintf("%q is a fixed infrastructure container", req.Name), http.StatusForbidden)
			return
		}
		if req.Image == "" {
			http.Error(w, "image is required", http.StatusBadRequest)
			return
		}
		if req.Host != "" && !validHostname(req.Host) {
			http.Error(w, "invalid hostname (allowed: a-z A-Z 0-9 . -, max 253 chars)", http.StatusBadRequest)
			return
		}
		if !validPort(req.Port) {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		if !validRoutePath(req.Path) {
			http.Error(w, "invalid path (must be empty or start with / — allowed: a-z A-Z 0-9 / _ . -, max 512 chars)", http.StatusBadRequest)
			return
		}
		if !validPort(req.PublishPort) {
			http.Error(w, "invalid publish_port", http.StatusBadRequest)
			return
		}
		if req.Service != "" && !validServiceName(req.Service) {
			http.Error(w, "invalid service (allowed: a-z A-Z 0-9 . _ -, max 63 chars)", http.StatusBadRequest)
			return
		}
		for _, u := range req.AuthUsers {
			if !validUsername(u) {
				http.Error(w, "invalid auth_users entry", http.StatusBadRequest)
				return
			}
		}
		if req.AuthMode != "" && req.AuthMode != "basic" && req.AuthMode != "oauth" {
			http.Error(w, "invalid auth_mode", http.StatusBadRequest)
			return
		}
		for _, vm := range req.VolumeMounts {
			if vm.Type != "volume" {
				http.Error(w, "volume_mounts entries must be type=volume", http.StatusBadRequest)
				return
			}
		}

		existing, err := dc.listAll(r.Context(), fmt.Sprintf(`{"name":["%s"]}`, req.Name))
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		for _, ct := range existing {
			if ct.name() == req.Name {
				http.Error(w, fmt.Sprintf("container %q already exists", req.Name), http.StatusConflict)
				return
			}
		}

		for _, v := range req.Volumes {
			if err := dc.createVolume(r.Context(), v); err != nil {
				httpx.WriteErr(w, err)
				return
			}
		}

		labels := map[string]string{
			labelEnable: "true",
			labelName:   req.Name,
			labelPort:   strconv.Itoa(req.Port),
		}
		if req.Host != "" {
			labels[labelHost] = req.Host
		}
		if req.Path != "" {
			labels[labelPath] = req.Path
		}
		if req.Strip {
			labels[labelStrip] = "true"
		}
		if req.Service != "" {
			labels[labelService] = req.Service
		} else {
			labels[labelService] = req.Name
		}
		if req.Unscalable {
			labels[labelUnscalable] = "true"
		}
		if req.Health != "" {
			labels[labelHealth] = req.Health
		}
		if req.Auth {
			labels[labelAuth] = "true"
		}
		if len(req.AuthUsers) > 0 {
			labels[labelAuthUsers] = strings.Join(req.AuthUsers, ",")
		}
		if req.AuthMode != "" {
			labels[labelAuthMode] = req.AuthMode
		}
		if req.RateLimit {
			labels[labelRateLimit] = "true"
		}
		if req.RateRPM > 0 {
			labels[labelRateRPM] = strconv.Itoa(req.RateRPM)
		}

		portKey := fmt.Sprintf("%d/tcp", req.Port)
		dc.pullImage(r.Context(), req.Image)
		id, err := dc.createContainer(r.Context(), req.Name, createBody{
			Image:        req.Image,
			Labels:       labels,
			Env:          req.Env,
			ExposedPorts: map[string]struct{}{portKey: {}},
			HostConfig: hostConfig{
				Mounts: req.VolumeMounts,
				// HostIP is always 127.0.0.1, never 0.0.0.0: this project's
				// convention is that nginx-stream (hand-configured per the
				// hint below) is the only sanctioned path to cross-host
				// reachability, so the published port itself must stay
				// unreachable from outside this host.
				PortBindings: map[string][]portBinding{portKey: {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(req.PublishPort)}}},
			},
		})
		if err != nil {
			if strings.Contains(err.Error(), "port is already allocated") || strings.Contains(err.Error(), "already in use") {
				httpx.WriteErr(w, fmt.Errorf("publish port %d is already in use on this host: %w", req.PublishPort, err))
				return
			}
			httpx.WriteErr(w, err)
			return
		}
		if err := dc.startContainer(r.Context(), id); err != nil {
			httpx.WriteErr(w, err)
			return
		}

		audit(r, "peer-mesh", "service.duplicate_receive", req.Name)
		httpx.WriteJSON(w, http.StatusOK, peerDuplicateResponse{Status: "created", Name: req.Name, Port: req.PublishPort})
	})
}
