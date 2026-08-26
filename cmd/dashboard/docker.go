package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	dockerSock = "/var/run/docker.sock"
	dockerAPI  = "v1.43"

	managedNetwork = "edge"

	labelEnable     = "proxy.enable"
	labelHost       = "proxy.host"
	labelPort       = "proxy.port"
	labelPath       = "proxy.path"
	labelStrip      = "proxy.strip"
	labelName       = "proxy.name"
	labelWeight     = "proxy.weight"
	labelService    = "proxy.service"
	labelGroup      = "proxy.group"          // product-level grouping for the Status view; defaults to the service name
	labelUnscalable = "proxy.unscalable"     // when "true", dashboard greys out +/- buttons
	labelPrevImage  = "proxy.previous_image" // set on Replace; enables one-click Rollback
	labelCanary     = "proxy.canary"         // "true" → staged replicas, served alongside live
	labelAutoUpdate = "proxy.autoupdate"     // "true" → engine replaces on newer registry digest
	labelHealth     = "proxy.health"         // optional HTTP health-check path, e.g. "/healthz"
	// labelMaintPage lives in maintpage.go, next to the sync that consumes it.

	// ociImageLabelPrefix marks labels that describe the IMAGE (baked in by
	// the build, e.g. org.opencontainers.image.revision), not the container.
	// replaceService must not carry these forward onto a replacement running
	// a different image.
	ociImageLabelPrefix = "org.opencontainers.image."
)

// dockerClient is the dashboard's READ-WRITE view of the Docker daemon.
// Required for creating/scaling/deleting services. Mount is rw in compose.
type dockerClient struct {
	http *http.Client
	// secrets resolves "ref:NAME" env edit values. Nil in the zero value
	// (including every test that builds a dockerClient by hand) — that's
	// fine, ref: edits just error out asking for SECRETS_DIR.
	secrets *secretsStore
}

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

func (c *dockerClient) do(ctx context.Context, method, path string, body any) (io.ReadCloser, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, "http://docker/"+dockerAPI+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("docker %s %s: %d %s", method, path, resp.StatusCode, string(b))
	}
	return resp.Body, nil
}

func (c *dockerClient) get(ctx context.Context, path string) (io.ReadCloser, error) {
	return c.do(ctx, "GET", path, nil)
}

type dockerContainer struct {
	ID              string            `json:"Id"`
	Names           []string          `json:"Names"`
	Image           string            `json:"Image"`
	ImageID         string            `json:"ImageID"`
	State           string            `json:"State"`
	Status          string            `json:"Status"` // raw docker status, e.g. "Up 2 minutes (healthy)"
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
	return c.ID[:12]
}

// parseHealth pulls a healthcheck state out of the raw Status string that
// /containers/json already returns (e.g. "Up 2 minutes (healthy)") — no
// extra /inspect call needed. Empty string means "no healthcheck defined",
// which callers should treat as healthy (most containers have none).
func parseHealth(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	default:
		return ""
	}
}

func (c *dockerClient) listAll(ctx context.Context, filter string) ([]dockerContainer, error) {
	q := "/containers/json?all=true"
	if filter != "" {
		q += "&filters=" + url.QueryEscape(filter)
	}
	body, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []dockerContainer
	return out, json.NewDecoder(body).Decode(&out)
}

func (c *dockerClient) listRunning(ctx context.Context, filter string) ([]dockerContainer, error) {
	q := "/containers/json"
	if filter != "" {
		q += "?filters=" + url.QueryEscape(filter)
	}
	body, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []dockerContainer
	return out, json.NewDecoder(body).Decode(&out)
}

// ---- Container lifecycle (for service management) ----

type createBody struct {
	Image            string              `json:"Image"`
	Labels           map[string]string   `json:"Labels,omitempty"`
	Env              []string            `json:"Env,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	Healthcheck      *healthcheckSpec    `json:"Healthcheck,omitempty"`
	HostConfig       hostConfig          `json:"HostConfig"`
	NetworkingConfig struct {
		EndpointsConfig map[string]struct{} `json:"EndpointsConfig"`
	} `json:"NetworkingConfig"`
}
type hostConfig struct {
	NetworkMode   string `json:"NetworkMode"`
	RestartPolicy struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
	Mounts       []mountSpec              `json:"Mounts,omitempty"`
	PortBindings map[string][]portBinding `json:"PortBindings,omitempty"`
}

// portBinding mirrors Docker Engine API's HostConfig.PortBindings entry
// shape — one host-side binding for a published container port.
type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// mountSpec mirrors Docker Engine API's HostConfig.Mounts entry shape —
// enough to carry a bind mount or named volume forward across a recreate.
type mountSpec struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

// healthcheckSpec mirrors Docker Engine API's Config.Healthcheck shape. A
// container's healthcheck is set by whatever created it (a compose file's
// `healthcheck:` stanza, most often) — it is NOT baked into the image, so a
// bare recreate from the image alone silently loses it unless the template
// container's own inspect result is carried forward explicitly.
type healthcheckSpec struct {
	Test        []string `json:"Test,omitempty"`
	Interval    int64    `json:"Interval,omitempty"`
	Timeout     int64    `json:"Timeout,omitempty"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
	Retries     int      `json:"Retries,omitempty"`
}

// cloneSpec is the subset of a container's HostConfig/Config that must ride
// forward when it's recreated (replace/stage/promote/scale/onboard) but
// isn't part of the labels/env already carried elsewhere.
type cloneSpec struct {
	Mounts      []mountSpec
	Healthcheck *healthcheckSpec
}

// pullImage tries to pull from a registry. Errors are non-fatal: if the image
// exists locally (built on the host, never pushed), createContainer will succeed
// anyway. The pull just makes sure we have the latest if it IS in a registry.
func (c *dockerClient) pullImage(ctx context.Context, image string) {
	ref := image
	tag := "latest"
	if i := strings.LastIndex(image, ":"); i != -1 && !strings.Contains(image[i:], "/") {
		ref = image[:i]
		tag = image[i+1:]
	}
	body, err := c.do(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(ref)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		log.Printf("pull %s skipped (probably a local image): %v", image, err)
		return
	}
	defer body.Close()
	_, _ = io.Copy(io.Discard, body)
}

// createVolume creates a named volume, POSTing to Docker's /volumes/create.
// Naturally idempotent: Docker returns 201 even if the volume already
// exists, so there's no existence pre-check.
func (c *dockerClient) createVolume(ctx context.Context, name string) error {
	resp, err := c.do(ctx, "POST", "/volumes/create", map[string]string{"Name": name})
	if err != nil {
		return err
	}
	resp.Close()
	return nil
}

func (c *dockerClient) createContainer(ctx context.Context, name string, body createBody) (string, error) {
	body.HostConfig.NetworkMode = managedNetwork
	body.HostConfig.RestartPolicy.Name = "unless-stopped"
	body.NetworkingConfig.EndpointsConfig = map[string]struct{}{managedNetwork: {}}
	resp, err := c.do(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), body)
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var out struct {
		ID string `json:"Id"`
	}
	return out.ID, json.NewDecoder(resp).Decode(&out)
}

// startContainer issues POST /containers/{id}/start. Docker returns
// 304 (Not Modified) when the container is already running — treat that
// as success since the caller's intent is satisfied either way. 404 is
// also tolerated so a race between listServices and the action (e.g.
// the container was just removed by something else) doesn't escalate
// into a user-visible error.
func (c *dockerClient) startContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", "/containers/"+id+"/start", nil)
	if err != nil {
		if strings.Contains(err.Error(), ": 304 ") || strings.Contains(err.Error(), ": 404 ") {
			return nil
		}
		return err
	}
	resp.Close()
	return nil
}

// stopContainer is the symmetric of startContainer. 304 = already stopped,
// 404 = already gone — both are fine for our intent.
func (c *dockerClient) stopContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", "/containers/"+id+"/stop?t=5", nil)
	if err != nil {
		if strings.Contains(err.Error(), ": 304 ") || strings.Contains(err.Error(), ": 404 ") {
			return nil
		}
		return err
	}
	resp.Close()
	return nil
}

func (c *dockerClient) removeContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "DELETE", "/containers/"+id+"?force=true", nil)
	if err != nil {
		return err
	}
	resp.Close()
	return nil
}

// ---- Local images (for the Images phase-out panel) ----

type dockerImage struct {
	Id          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Size        int64    `json:"Size"`
	SharedSize  int64    `json:"SharedSize"`
	Created     int64    `json:"Created"`
	Containers  int64    `json:"Containers"`
}

func (c *dockerClient) listImages(ctx context.Context) ([]dockerImage, error) {
	body, err := c.get(ctx, "/images/json?shared-size=true")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []dockerImage
	return out, json.NewDecoder(body).Decode(&out)
}

// removeImage deletes a LOCAL image. token is a repo:tag reference (untag
// semantics — safe when the same ID has other tags) or an image ID (used only
// for dangling images). force is always passed through as-is; callers keep it
// false so the daemon's own in-use refusal stays as the backstop.
func (c *dockerClient) removeImage(ctx context.Context, token string, force bool) error {
	resp, err := c.do(ctx, "DELETE",
		"/images/"+url.PathEscape(token)+"?force="+strconv.FormatBool(force)+"&noprune=false", nil)
	if err != nil {
		if strings.Contains(err.Error(), ": 409 ") {
			return fmt.Errorf("image %s is in use or has other tags — not removed", token)
		}
		return err
	}
	defer resp.Close()
	_, _ = io.Copy(io.Discard, resp)
	return nil
}

// inspectContainer returns just the Env slice for a given container — needed
// when scaling so the new replica gets the same runtime config as the template.
func (c *dockerClient) inspectEnv(ctx context.Context, id string) ([]string, error) {
	body, err := c.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var resp struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, err
	}
	return resp.Config.Env, nil
}

// inspectRestartCount returns how many times Docker has auto-restarted a
// container since it was created — used by the rollout health gate as a
// signal a healthcheck-less (or slow-to-fail) canary is actually crash
// looping.
func (c *dockerClient) inspectRestartCount(ctx context.Context, id string) (int, error) {
	body, err := c.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return 0, err
	}
	defer body.Close()
	var resp struct {
		RestartCount int `json:"RestartCount"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return 0, err
	}
	return resp.RestartCount, nil
}

// inspectCloneSpec returns the HostConfig/Config fields a recreate must carry
// forward — Mounts (bind mounts / named volumes) and Healthcheck — which
// createBody drops today if the caller doesn't thread them through.
func (c *dockerClient) inspectCloneSpec(ctx context.Context, id string) (cloneSpec, error) {
	body, err := c.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return cloneSpec{}, err
	}
	defer body.Close()
	var resp struct {
		HostConfig struct {
			Mounts []mountSpec `json:"Mounts"`
		} `json:"HostConfig"`
		Config struct {
			Healthcheck *healthcheckSpec `json:"Healthcheck"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return cloneSpec{}, err
	}
	return cloneSpec{Mounts: resp.HostConfig.Mounts, Healthcheck: resp.Config.Healthcheck}, nil
}

// looksLikeBareDigest reports whether s is a bare "sha256:<hex>" image
// reference with no repo/tag component — the shape Docker's /containers/json
// list endpoint's "Image" field decays into once the tag a container was
// created from is later retagged or removed locally. A proper reference
// (repo:tag, or repo@sha256:<hex>) never matches this.
func looksLikeBareDigest(image string) bool {
	return strings.HasPrefix(image, "sha256:")
}

// inspectConfigImage returns Config.Image from a container's inspect — the
// exact reference (e.g. "myapp:latest") passed to `docker create` at
// creation time, immutable regardless of what a tag is later repointed to.
// Contrast with dockerContainer.Image (the /containers/json list endpoint's
// "Image" field), which Docker degrades to a bare image ID/digest once the
// creating tag is retagged or removed locally.
//
// Verified live (2026-08-25) against the raw Engine API: after tagging,
// creating, and starting a container, then removing its creating tag, the
// list endpoint's Image field decayed to "sha256:<digest>" while this
// container's own /containers/{id}/json Config.Image stayed the original
// "name:tag" reference used at creation. No currently-decayed container
// existed on the Pi or Mac to spot-check directly, so the decay was
// reproduced synthetically rather than observed on a real workload.
func (c *dockerClient) inspectConfigImage(ctx context.Context, id string) (string, error) {
	body, err := c.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return "", err
	}
	defer body.Close()
	var resp struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return "", err
	}
	return resp.Config.Image, nil
}

// hostConfigRefuseFields is a CLOSED allowlist of HostConfig field names that,
// if set to a non-zero value, mean the container carries config a recreate
// (createContainer, Phase 0's Mounts aside) has no way to reproduce — refuse
// onboarding rather than silently drop it.
//
// This is deliberately NOT "every field decoded from a generic
// map[string]json.RawMessage" — a real `docker inspect` HostConfig always
// carries plenty of daemon-populated, present-but-zero-in-spirit fields
// (LogConfig, MaskedPaths, ReadonlyPaths, ShmSize, Runtime, CgroupnsMode,
// IpcMode, ConsoleSize, ...) that are non-zero JSON values on literally every
// container, labeled or not. Refusing on any of those would refuse 100% of
// containers, so only the fields below — the ones a container could only
// have picked up via an explicit `docker run` flag or compose option that
// createContainer cannot carry forward — are checked. NetworkMode and
// RestartPolicy are intentionally excluded: createContainer already
// hardcodes/manages both. Mounts is intentionally excluded: Phase 0's
// inspectCloneSpec carries it forward.
var hostConfigRefuseFields = []string{
	"PortBindings",
	"Binds",
	"Dns", "DnsSearch", "DnsOptions",
	"ExtraHosts",
	"CapAdd", "CapDrop",
	"Privileged",
	"Devices",
	"Memory", "MemorySwap", "NanoCpus", "CpuShares", "CpusetCpus", "CpusetMems", "PidsLimit",
}

// isZeroJSON reports whether a raw JSON value is "not really set": null,
// false, 0, "", [], or {}. Needed because encoding/json decodes into
// map[string]json.RawMessage without dropping unknown/zero fields the way a
// typed struct decode silently would — every field docker inspect returns is
// present in the map, just often zero-valued, and a naive "key exists in the
// map" check would refuse every container.
func isZeroJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true // field absent from the JSON object entirely
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case float64:
		return t == 0
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// jsonEqual compares two raw JSON values by structural equality rather than
// byte-for-byte — needed to compare a container's Config.Cmd/Entrypoint/
// Healthcheck against its image's, where key order isn't guaranteed to match.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}

// inspectHostConfigUnknowns fetches a container's inspect output and returns
// the names of any HostConfig/Config/NetworkSettings fields it carries that a
// recreate (onboarding, replace, autoupdate) has no way to reproduce — the
// caller should refuse rather than silently drop them.
//
// Decoded into generic maps (json.RawMessage), not a typed struct: a typed
// decode silently drops fields it doesn't know about, and Docker's inspect
// response returns every field present-but-often-zero-valued, which would
// make a naive "is this field present" check refuse everything.
//
// Config.Cmd/Entrypoint need special handling: Docker populates Config.Cmd
// from the IMAGE's own CMD for virtually every container (e.g.
// ["nginx","-g","daemon off;"]), so there is no way to tell "image default"
// from "user override" from the container's inspect alone. This fetches the
// image's own inspect and only refuses when the container's value actually
// differs from what the image would produce on its own.
//
// Config.Healthcheck is deliberately NOT checked here: inspectCloneSpec
// carries it forward on every recreate path (see cloneSpec), so unlike
// Cmd/Entrypoint it no longer needs a refuse-rather-than-drop guard.
//
// TODO(pi-verification): the field lists above were built from the public,
// documented Docker Engine API HostConfig/Config schema, not a live spot
// check — re-verify against a real `docker inspect` on the Pi once it's back
// up, in case a Docker Engine version in use here shapes any field
// differently.
func (c *dockerClient) inspectHostConfigUnknowns(ctx context.Context, id string) ([]string, error) {
	body, err := c.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var resp struct {
		Image      string                     `json:"Image"` // image ID, e.g. "sha256:..."
		HostConfig map[string]json.RawMessage `json:"HostConfig"`
		Config     struct {
			Cmd        json.RawMessage `json:"Cmd"`
			Entrypoint json.RawMessage `json:"Entrypoint"`
		} `json:"Config"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, err
	}

	var refused []string
	for _, f := range hostConfigRefuseFields {
		raw, ok := resp.HostConfig[f]
		if !ok || isZeroJSON(raw) {
			continue
		}
		refused = append(refused, f)
	}

	for netName := range resp.NetworkSettings.Networks {
		if netName != managedNetwork {
			refused = append(refused, "NetworkSettings.Networks."+netName)
		}
	}

	// Config.Cmd/Entrypoint: only refuse when they DIFFER from what the image
	// itself already specifies.
	if !isZeroJSON(resp.Config.Cmd) || !isZeroJSON(resp.Config.Entrypoint) {
		imgCmd, imgEntrypoint, err := c.inspectImageOverridable(ctx, resp.Image)
		if err != nil {
			return nil, fmt.Errorf("inspect image %s: %w", resp.Image, err)
		}
		if !isZeroJSON(resp.Config.Cmd) && !jsonEqual(resp.Config.Cmd, imgCmd) {
			refused = append(refused, "Config.Cmd")
		}
		if !isZeroJSON(resp.Config.Entrypoint) && !jsonEqual(resp.Config.Entrypoint, imgEntrypoint) {
			refused = append(refused, "Config.Entrypoint")
		}
	}

	sort.Strings(refused)
	return refused, nil
}

// inspectImageOverridable returns an image's own Config.Cmd/Entrypoint — the
// baseline inspectHostConfigUnknowns diffs a container's values against, so a
// container that simply inherited them from its image isn't mistaken for one
// with an explicit override.
func (c *dockerClient) inspectImageOverridable(ctx context.Context, imageID string) (cmd, entrypoint json.RawMessage, err error) {
	body, err := c.get(ctx, "/images/"+url.PathEscape(imageID)+"/json")
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	var resp struct {
		Config struct {
			Cmd        json.RawMessage `json:"Cmd"`
			Entrypoint json.RawMessage `json:"Entrypoint"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, nil, err
	}
	return resp.Config.Cmd, resp.Config.Entrypoint, nil
}

// ---- Service-level operations ----

type Service struct {
	Name string `json:"name"`
	// Group is the product-level grouping (proxy.group label) the Status
	// view/statusbot bucket services by — defaults to Name when the label is
	// absent, so an ungrouped service is its own group of one.
	Group      string `json:"group"`
	Image      string `json:"image"`
	ImageID    string `json:"image_id,omitempty"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Path       string `json:"path,omitempty"`
	Replicas   int    `json:"replicas"`
	Unscalable bool   `json:"unscalable,omitempty"`
	// Weight is proxy.weight, the per-replica routing weight within the
	// proxy's pool for this route — and, since the peer mesh advertises the
	// sum of them, this service's share of cross-host spread traffic too.
	// Always populated (1 when the label is absent or unparseable, matching
	// cmd/proxy/router.go's own default) so the UI can render a real number
	// rather than an empty box.
	Weight          int    `json:"weight,omitempty"`
	PreviousImage   string `json:"previous_image,omitempty"`   // for one-click rollback
	UpdateAvailable bool   `json:"update_available,omitempty"` // set by image checker
	// ImageCheckError mirrors the image-checker's last error for this
	// service's Image (imageStatus.Err in images.go). Non-empty means the
	// registry/distribution check has been failing — which also means
	// UpdateAvailable can never flip true until it's fixed. Empty when the
	// last check succeeded or hasn't run yet.
	ImageCheckError string `json:"image_check_error,omitempty"`
	// AutoUpdateSkipReason is set only when UpdateAvailable is true and the
	// service still won't be picked up by the next auto-update tick — see
	// autoUpdateSkipReason (autoupdate.go). Empty whenever there's nothing to
	// explain (no update pending, or it actually will fire).
	AutoUpdateSkipReason string `json:"autoupdate_skip_reason,omitempty"`
	AutoUpdate           bool   `json:"auto_update,omitempty"`  // opted in to unattended updates
	CanaryImage          string `json:"canary_image,omitempty"` // non-empty when a stage is in progress
	CanaryReplicas       int    `json:"canary_replicas,omitempty"`
	Onboarded            bool   `json:"onboarded,omitempty"` // adopted from an unlabelled container
	// DualTracked is true when this service is BOTH still label-managed (a
	// running container carries proxy.* labels) AND has an onboarded record
	// tracking it — the state that let the sfubadminton.com incident happen
	// (onboarded.go's promoteToOnboarded once silently dropped Path/Strip
	// when snapshotting a still-labeled container). Not itself an error —
	// it's how a label-managed service picks up Stage/Promote/Replace/
	// Rollback — but every field the two representations both track has to
	// stay reconciled, so it's worth surfacing rather than only discoverable
	// by diffing routes.json against docker labels by hand.
	DualTracked     bool              `json:"dual_tracked,omitempty"`
	Members         []dockerContainer `json:"-"`
	Labels          map[string]string `json:"labels,omitempty"`
	MemberSummaries []ServiceMember   `json:"member_summaries"`      // per-replica name/state for UI
	AllStopped      bool              `json:"all_stopped,omitempty"` // true if every non-canary member is stopped
	// Backends are the upstream URLs the proxy picks for this service, in the
	// same "http://ip:port" form it records in the access log. Several
	// services can share one host (badminton.polardev.org fans out to four),
	// so host alone cannot attribute a request — the UI matches on these.
	Backends []string `json:"backends,omitempty"`
	// Machine is the peer identity (DASHBOARD_HOST, see peers.go) whose
	// dashboard instance this service's data came from — "" for this host's
	// own services (the common case, and the only case before any peers are
	// configured), set only on services merged in from a peer's
	// /peer/services. Mirrors ServiceStatusGroup.Machine in servicestatus.go.
	Machine string `json:"machine,omitempty"`
}

// ServiceMember is one container's surface for the UI — name (DNS-routable),
// state ("running", "exited", "created", "paused"…), and whether it's the
// canary or a normal replica.
type ServiceMember struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	State    string `json:"state"`
	IsCanary bool   `json:"is_canary,omitempty"`
	// Health is parsed from the container's raw Status string: "healthy",
	// "unhealthy", "starting", or "" when no healthcheck is defined (most
	// containers) — treat "" as healthy, not unknown.
	Health string `json:"health,omitempty"`
}

func (c *dockerClient) listServices(ctx context.Context) ([]Service, error) {
	containers, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s"]}`, labelService))
	if err != nil {
		return nil, err
	}
	byName := map[string]*Service{}
	for _, ct := range containers {
		name := ct.Labels[labelService]
		if !validServiceName(name) {
			// Reject rogue-image XSS: a container whose `proxy.service` label
			// contains anything outside serviceNameRE never reaches the UI.
			log.Printf("skip container %s: invalid proxy.service label %q", ct.name(), name)
			continue
		}
		host := ct.Labels[labelHost]
		if host != "" && !validHostname(host) {
			log.Printf("skip container %s: invalid proxy.host label %q", ct.name(), host)
			continue
		}
		if p := ct.Labels[labelPath]; p != "" && !validProxyPath(p) {
			log.Printf("skip container %s: invalid proxy.path label %q", ct.name(), p)
			continue
		}
		group := ct.Labels[labelGroup]
		if group != "" && !validServiceName(group) {
			// Rendered into an HTML heading client-side on the Status tab —
			// same XSS boundary as proxy.service/proxy.host above.
			log.Printf("skip container %s: invalid proxy.group label %q", ct.name(), group)
			continue
		}
		isCanary := ct.Labels[labelCanary] == "true"
		s, ok := byName[name]
		if !ok {
			s = &Service{Name: name, Labels: ct.Labels}
			byName[name] = s
		}
		if s.Group == "" {
			s.Group = group
		}
		s.Members = append(s.Members, ct)
		if isCanary {
			s.CanaryImage = ct.Image
			s.CanaryReplicas++
		} else {
			port, _ := strconv.Atoi(ct.Labels[labelPort])
			img := ct.Image
			if looksLikeBareDigest(img) {
				if ref, err := c.inspectConfigImage(ctx, ct.ID); err == nil && ref != "" && !looksLikeBareDigest(ref) {
					img = ref
				}
			}
			s.Image = img
			s.ImageID = ct.ImageID
			s.Host = host
			s.Port = port
			s.Path = ct.Labels[labelPath]
			s.Unscalable = ct.Labels[labelUnscalable] == "true"
			s.Weight = parseWeightLabel(ct.Labels[labelWeight])
			s.PreviousImage = ct.Labels[labelPrevImage]
			s.AutoUpdate = ct.Labels[labelAutoUpdate] == "true"
			s.Replicas++
		}
	}
	out := make([]Service, 0, len(byName))
	for _, s := range byName {
		if s.Group == "" {
			s.Group = s.Name
		}
		// Build per-member summaries (sorted by name for stable UI order)
		// and determine the AllStopped flag from non-canary members.
		allStopped := len(s.Members) > 0
		for _, m := range s.Members {
			isCanary := m.Labels[labelCanary] == "true"
			s.MemberSummaries = append(s.MemberSummaries, ServiceMember{
				Name: m.name(), ID: m.ID, State: m.State, IsCanary: isCanary, Health: parseHealth(m.Status),
			})
			if !isCanary && m.State == "running" {
				allStopped = false
			}
		}
		sort.Slice(s.MemberSummaries, func(i, j int) bool { return s.MemberSummaries[i].Name < s.MemberSummaries[j].Name })
		s.AllStopped = allStopped
		s.Backends = serviceBackends(s)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mergeOnboardedLiveState folds a dual-tracked service's onboarded clone
// containers (goproxy-onb-<name>-*, which deliberately carry no proxy.*
// labels — that's what makes them "onboarded" rather than label-managed)
// into its Members/AllStopped. Without this, a dual-tracked Service's status
// reflects ONLY whatever label-carrying container listServices found under
// the same name — including a stale, long-exited leftover from a prior
// relabel/adopt operation — so a fully-live onboarded replica can report
// AllStopped=true because listServices never saw it at all (2026-08-26:
// badminton-staging-player/-admin showed "down" for a week off a dead
// "-relabel" container while the real replica ran fine).
func (c *dockerClient) mergeOnboardedLiveState(ctx context.Context, s *Service) {
	clones, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, s.Name))
	if err != nil || len(clones) == 0 {
		return
	}
	seen := make(map[string]bool, len(s.Members))
	for _, m := range s.Members {
		seen[m.ID] = true
	}
	anyRunning := !s.AllStopped
	for _, cl := range clones {
		if seen[cl.ID] {
			continue
		}
		s.Members = append(s.Members, cl)
		s.MemberSummaries = append(s.MemberSummaries, ServiceMember{
			Name: cl.name(), ID: cl.ID, State: cl.State, Health: parseHealth(cl.Status),
		})
		if cl.State == "running" {
			anyRunning = true
		}
	}
	s.AllStopped = !anyRunning
	sort.Slice(s.MemberSummaries, func(i, j int) bool { return s.MemberSummaries[i].Name < s.MemberSummaries[j].Name })
}

type CreateServiceRequest struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Host       string            `json:"host"`
	Port       int               `json:"port"`
	Path       string            `json:"path,omitempty"`
	Strip      bool              `json:"strip,omitempty"`
	Replicas   int               `json:"replicas"`
	Unscalable bool              `json:"unscalable,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// replaceSettleDelay is how long replaceService lets new containers come up
// before removing the old ones.
var replaceSettleDelay = 5 * time.Second

// rollingReadyTimeout bounds waitReplicaReady's health gate for a single
// freshly-created replica. Deliberately its own constant rather than reusing
// canaryPromoteHealthTimeout (30s): that value was tuned for a canary that's
// already been running for a while before promotion is even attempted, but
// waitReplicaReady's container is brand new — its Docker healthcheck can
// legitimately still report "(health: starting)" past 30s depending on the
// image's configured start_period/interval (observed on badminton-admin:
// StartPeriod 20s + Interval 30s puts the first health verdict right at the
// 30s boundary). The failure modes are asymmetric: too short aborts a
// healthy rollout mid-loop, too long only delays declaring a genuinely
// broken replica dead — and since surge-of-one never drops capacity below
// the original count, a slower failure is not an outage. A generous fixed
// timeout dominates trying to derive one from the image's healthcheck
// config. Keep N replicas * (rollingReadyTimeout + replaceSettleDelay)
// comfortably under rollingOpTimeout.
var rollingReadyTimeout = 3 * time.Minute

// ReplaceServiceRequest swaps a service's image (and optionally env) in place.
// Spins up new containers first, briefly waits, then removes the old ones —
// approximation of a rolling deploy on a single host.
type ReplaceServiceRequest struct {
	Image string `json:"image"` // required
	// Env carries EDITS, not a replacement set: entries are merged onto the
	// env the service is currently running (see serviceenv.go). Nil/empty
	// means "keep the current env exactly", which is what the auto-updater
	// sends. A key whose value disagrees with the running one is refused as
	// an envConflictError until the caller acknowledges it below.
	Env map[string]string `json:"env,omitempty"`
	// EnvAck lists keys the caller was shown a conflict for and deliberately
	// chose to overwrite. Anything not listed still conflicts.
	EnvAck []string `json:"env_ack,omitempty"`
}

// guardUnscalable refuses to scale a service that has any container labeled
// proxy.unscalable=true above 1 replica. Returns nil if scaling is allowed.
func (c *dockerClient) guardUnscalable(ctx context.Context, name string, desired int) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return nil
	}
	// Strip canaries and prefer a running member — same reasoning as
	// preferRunning's other call sites: a stale exited leftover (e.g. one
	// predating the addition of proxy.unscalable to the compose file) could
	// otherwise sort first and silently lack the label, letting an
	// unscalable service get scaled past 1 with no error.
	existing := preferRunning(liveOnly(all))
	if len(existing) == 0 {
		return nil
	}
	if existing[0].Labels[labelUnscalable] == "true" && desired != 1 {
		return fmt.Errorf("%q is marked unscalable — replica count must stay at 1", name)
	}
	return nil
}

func (c *dockerClient) scaleService(ctx context.Context, name string, desired int) error {
	if desired < 0 {
		return fmt.Errorf("replicas must be >= 0")
	}
	if err := c.guardUnscalable(ctx, name, desired); err != nil {
		return err
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return fmt.Errorf("service %q not found (no live replicas)", name)
	}
	// Prefer a RUNNING container as the template so a stale exited leftover
	// doesn't donate its (possibly stale) env/mounts to newly-scaled-up
	// replicas. current/desired arithmetic below still counts the full
	// existing set — it drives scale-down's removal-count guard, which
	// operates over the same full set, and scale-up's index generation
	// still needs to see stale names to avoid a create-time 409.
	tpl := preferRunning(existing)[0]
	current := len(existing)
	switch {
	case current == desired:
		return nil
	case current < desired:
		// Pull the template's env so replicas get the same runtime config
		// (DATABASE_URL, API keys, etc.) — the listAll summary doesn't include Env.
		env, err := c.inspectEnv(ctx, tpl.ID)
		if err != nil {
			return fmt.Errorf("inspect template %s: %w", tpl.name(), err)
		}
		clone, err := c.inspectCloneSpec(ctx, tpl.ID)
		if err != nil {
			return fmt.Errorf("inspect template %s: %w", tpl.name(), err)
		}
		for i := 0; i < desired-current; i++ {
			n := nextReplicaIndex(existing, name) + i
			cname := fmt.Sprintf("goproxy-%s-%d", name, n)
			id, err := c.createContainer(ctx, cname, createBody{Image: tpl.Image, Labels: tpl.Labels, Env: env, Healthcheck: clone.Healthcheck, HostConfig: hostConfig{Mounts: clone.Mounts}})
			if err != nil {
				return fmt.Errorf("create %s: %w", cname, err)
			}
			if err := c.startContainer(ctx, id); err != nil {
				return fmt.Errorf("start %s: %w", cname, err)
			}
		}
	default:
		// Scale down: only remove containers WE created (prefix "goproxy-").
		// Never touch docker-compose-managed originals — they hold the canonical
		// env vars / volumes and are the source of truth for re-scaling later.
		toRemove := current - desired
		var ours []dockerContainer
		for _, ct := range existing {
			if strings.HasPrefix(ct.name(), "goproxy-") {
				ours = append(ours, ct)
			}
		}
		// Reclaim a stopped replica before touching a running one — the UI's
		// stepper displays the running count (a stopped member already reads
		// as "not there"), so scaling down must actually remove the member
		// that's already down first rather than killing a live one and
		// leaving the stale stopped one behind. Ties (same state) still
		// remove highest-indexed first (e.g. goproxy-foo-5 before goproxy-foo-2).
		sort.Slice(ours, func(i, j int) bool {
			iRunning, jRunning := ours[i].State == "running", ours[j].State == "running"
			if iRunning != jRunning {
				return !iRunning
			}
			return ours[i].name() > ours[j].name()
		})
		if len(ours) < toRemove {
			return fmt.Errorf("can only scale down to %d (the original is not removable)", current-len(ours))
		}
		for i := 0; i < toRemove; i++ {
			_ = c.stopContainer(ctx, ours[i].ID)
			if err := c.removeContainer(ctx, ours[i].ID); err != nil {
				return fmt.Errorf("remove %s: %w", ours[i].name(), err)
			}
		}
	}
	return nil
}

func nextReplicaIndex(existing []dockerContainer, service string) int {
	max := 0
	prefix := "goproxy-" + service + "-"
	for _, ct := range existing {
		n := ct.name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		if v, err := strconv.Atoi(n[len(prefix):]); err == nil && v > max {
			max = v
		}
	}
	return max + 1
}

func (c *dockerClient) createService(ctx context.Context, req CreateServiceRequest) error {
	if req.Name == "" || req.Image == "" || req.Host == "" || req.Port == 0 {
		return fmt.Errorf("name, image, host, and port are required")
	}
	if !validServiceName(req.Name) {
		return fmt.Errorf("invalid service name (allowed: a-z A-Z 0-9 . _ -, max 63 chars)")
	}
	if !validHostname(req.Host) {
		return fmt.Errorf("invalid hostname (allowed: a-z A-Z 0-9 . -, max 253 chars)")
	}
	if req.Path != "" && !validProxyPath(req.Path) {
		return fmt.Errorf("invalid path (must start with /, allowed: A-Z a-z 0-9 / _ . -)")
	}
	if !validPort(req.Port) {
		return fmt.Errorf("invalid port")
	}
	if req.Replicas < 1 {
		req.Replicas = 1
	}
	c.pullImage(ctx, req.Image)
	labels := map[string]string{
		labelEnable:  "true",
		labelHost:    req.Host,
		labelPort:    strconv.Itoa(req.Port),
		labelService: req.Name,
		labelName:    req.Name,
	}
	if req.Path != "" {
		labels[labelPath] = req.Path
	}
	if req.Strip {
		labels[labelStrip] = "true"
	}
	if req.Unscalable {
		labels[labelUnscalable] = "true"
	}
	// No merge/conflict here — there's no running container to compare
	// against yet, just resolution. refs (the conflict-redaction map) is
	// unused for the same reason: nothing here can produce an
	// envConflictError.
	resolvedEnv, _, err := resolveSecretRefs(req.Name, req.Env, c.secrets)
	if err != nil {
		return err
	}
	var env []string
	for k, v := range resolvedEnv {
		env = append(env, k+"="+v)
	}
	for i := 1; i <= req.Replicas; i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", req.Name, i)
		id, err := c.createContainer(ctx, cname, createBody{Image: req.Image, Labels: labels, Env: env})
		if err != nil {
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			return fmt.Errorf("start %s: %w", cname, err)
		}
	}
	return nil
}

// liveOnly returns containers that aren't canary (the active production set).
func liveOnly(in []dockerContainer) []dockerContainer {
	out := in[:0:0]
	for _, ct := range in {
		if ct.Labels[labelCanary] != "true" {
			out = append(out, ct)
		}
	}
	return out
}
func canaryOnly(in []dockerContainer) []dockerContainer {
	out := in[:0:0]
	for _, ct := range in {
		if ct.Labels[labelCanary] == "true" {
			out = append(out, ct)
		}
	}
	return out
}

// runningOnly returns containers whose State == "running". listAll always
// queries Docker with all=true (so a fully-scaled-to-zero service can still
// be found), and liveOnly only strips canary-labeled containers — neither
// filters on State. Without this, a STOPPED/EXITED leftover (e.g. one whose
// removal silently failed during a prior replace) can sit in the label-
// filtered result indefinitely and get treated as equivalent to a live one.
func runningOnly(in []dockerContainer) []dockerContainer {
	out := in[:0:0]
	for _, ct := range in {
		if ct.State == "running" {
			out = append(out, ct)
		}
	}
	return out
}

// preferRunning returns the RUNNING subset of in when it's non-empty, so
// template selection (env/mounts/labels) and replica-count preservation pick
// a live container instead of a stale exited one that happens to sort first.
// Falls back to the full set when every member is stopped, preserving the
// existing (intentional) ability to act on a fully-stopped service — see
// Service.AllStopped and onboardedBaseEnv's "better a stale base than no
// base" doc comment for the same philosophy.
func preferRunning(in []dockerContainer) []dockerContainer {
	if running := runningOnly(in); len(running) > 0 {
		return running
	}
	return in
}

// replaceService creates fresh containers with a new image (and optionally new
// env), starts them, waits briefly, then removes the old containers. Replica
// count is preserved. Labels are inherited from the existing template.
// replaceTemplate is the resolved recreate plan prepareReplaceTemplate
// produces: everything both replaceService's all-at-once tail and
// replaceServiceRolling's surge-of-one tail need to actually create and swap
// containers, without either of them re-deriving it (and possibly
// disagreeing on env, labels, or the starting replica index).
type replaceTemplate struct {
	existing  []dockerContainer
	tplSet    []dockerContainer
	env       []string
	clone     cloneSpec
	newLabels map[string]string
	startIdx  int
}

// prepareReplaceTemplate resolves everything a label-managed service replace
// needs up front — the live/template container sets, the host-config-drop
// refusal check, merged env, the pulled image, the new containers' labels,
// and the starting replica index (computed ONCE here, not per-replica by a
// caller's loop: recomputing after removing an old container could hand out
// an index still held by an unreleased container and 409 on create) — so
// replaceService and replaceServiceRolling share one source of truth instead
// of two copies that could drift.
func (c *dockerClient) prepareReplaceTemplate(ctx context.Context, name string, req ReplaceServiceRequest) (*replaceTemplate, error) {
	if req.Image == "" {
		return nil, fmt.Errorf("image is required")
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return nil, err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return nil, fmt.Errorf("service %q not found (no live replicas)", name)
	}
	// tplSet prefers RUNNING containers for both template selection and the
	// recreate count below — a stale exited leftover (e.g. one whose removal
	// silently failed on a prior replace, see the teardown loop's log-only
	// error handling near the bottom of this function) must not donate its
	// possibly-stale env as the template, nor inflate the replica count of
	// the replacement set. Falls back to the full existing set when every
	// member is stopped, so replacing an all-stopped service still works.
	tplSet := preferRunning(existing)
	tpl := tplSet[0]

	// Refuse rather than silently strip host config a recreate can't
	// reproduce — see hostConfigRefuseFields's doc comment. This is the
	// self-inflicted-outage guard: without it, replacing a container that
	// publishes host ports (e.g. this dashboard's own :8093/:8098) creates
	// a portless replacement, then removes the old container that actually
	// held the bindings, taking the service unreachable with no error.
	for _, ct := range existing {
		unknowns, err := c.inspectHostConfigUnknowns(ctx, ct.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", ct.name(), err)
		}
		if len(unknowns) > 0 {
			return nil, fmt.Errorf("refusing to replace %q: %s would drop %s on recreate — resolve manually first", name, ct.name(), strings.Join(unknowns, ", "))
		}
	}

	// Resolve env by merging any edits onto what the template is running.
	// Read unconditionally: the merge needs the current values to compare
	// against, not just as a fallback when no edits were sent.
	base, err := c.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return nil, fmt.Errorf("inspect template env: %w", err)
	}
	clone, err := c.inspectCloneSpec(ctx, tpl.ID)
	if err != nil {
		return nil, fmt.Errorf("inspect template clone spec: %w", err)
	}
	edits, refs, err := resolveSecretRefs(name, req.Env, c.secrets)
	if err != nil {
		return nil, err
	}
	env, err := mergeEnv(base, edits, req.EnvAck)
	if err != nil {
		return nil, redactRefConflicts(err, refs)
	}

	c.pullImage(ctx, req.Image)

	// Stamp the new containers' labels with the previous image for one-click rollback.
	newLabels := map[string]string{}
	for k, v := range tpl.Labels {
		// org.opencontainers.image.* describes the IMAGE, not the container —
		// carrying the old image's copy forward would override (not merely
		// shadow) the new image's own labels of the same key at create time,
		// leaving e.g. image.revision pointing at the image being replaced.
		// Omit it here so Docker fills it in from req.Image instead.
		if strings.HasPrefix(k, ociImageLabelPrefix) {
			continue
		}
		newLabels[k] = v
	}
	if tpl.Image != "" && tpl.Image != req.Image {
		newLabels[labelPrevImage] = tpl.Image
	}

	// Create N replacements. N intentionally reflects tplSet (running-
	// preferred), not the raw existing count — so a stale exited leftover
	// doesn't get "replaced" too and inflate the live replica count.
	// startIdx still scans the full existing set (not tplSet): it must see
	// every name Docker currently holds, running or not, or it could hand
	// out an index still occupied by a not-yet-removed stale container and
	// 409 on create.
	startIdx := nextReplicaIndex(existing, name)

	return &replaceTemplate{
		existing:  existing,
		tplSet:    tplSet,
		env:       env,
		clone:     clone,
		newLabels: newLabels,
		startIdx:  startIdx,
	}, nil
}

func (c *dockerClient) replaceService(ctx context.Context, name string, req ReplaceServiceRequest) error {
	tpl, err := c.prepareReplaceTemplate(ctx, name, req)
	if err != nil {
		return err
	}

	var newIDs []string
	for i := 0; i < len(tpl.tplSet); i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", name, tpl.startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image:       req.Image,
			Labels:      tpl.newLabels,
			Env:         tpl.env,
			Healthcheck: tpl.clone.Healthcheck,
			HostConfig:  hostConfig{Mounts: tpl.clone.Mounts},
		})
		if err != nil {
			// Roll back: tear down any new ones we already created.
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("start %s: %w", cname, err)
		}
		newIDs = append(newIDs, id)
	}

	// Give the new containers a few seconds to bind their ports / accept connections
	// before we tear down the old. Crude — production would health-check here.
	// A package var only so tests can shrink it; nothing else reassigns it.
	time.Sleep(replaceSettleDelay)

	for _, ct := range tpl.existing {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("replace %s: failed to remove old %s: %v (new ones are running)", name, ct.name(), err)
		}
	}
	return nil
}

// waitReplicaReady gates replaceServiceRolling's create-before-destroy swap:
// it polls checkContainerHealthy (docker healthcheck, restart count, and any
// proxy.health probe) every canaryPromoteHealthPoll up to
// rollingReadyTimeout, re-listing the container fresh from Docker each
// poll (its Status/NetworkSettings/restart count all change over its
// lifetime, so a single snapshot taken at create time would go stale and the
// health gate would pass vacuously). Once healthy, it additionally requires
// the container to have been running for at least replaceSettleDelay (the
// same dwell replaceService's all-at-once tail already uses — reused, not
// duplicated) before returning, so a container with neither a docker
// healthcheck nor a proxy.health label — which checkContainerHealthy passes
// instantly — still isn't trusted with its predecessor's traffic the moment
// it starts.
func (c *dockerClient) waitReplicaReady(ctx context.Context, name, id string) error {
	started := time.Now()
	deadline := started.Add(rollingReadyTimeout)
	var lastReason string
	for {
		all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
		if err != nil {
			return fmt.Errorf("health check: %w", err)
		}
		var ct *dockerContainer
		for i := range all {
			if all[i].ID == id {
				ct = &all[i]
				break
			}
		}
		if ct == nil {
			return fmt.Errorf("container disappeared while waiting for it to become healthy")
		}
		// checkContainerHealthy only treats docker's own "(unhealthy)" status
		// as a failure — "(health: starting)" (the status every container
		// with a docker healthcheck reports until its first probe completes,
		// which can be well past this container's start time depending on
		// the healthcheck's configured interval) is not "unhealthy" and
		// would otherwise pass the gate instantly. Treat it as not-yet-ready
		// here instead, without changing checkContainerHealthy itself — that
		// function is also checkCanaryHealth's per-container check, and this
		// rolling-replace-specific gate must not change canary behavior.
		if parseHealth(ct.Status) == "starting" {
			lastReason = fmt.Sprintf("%s: healthcheck still starting", ct.name())
			if !time.Now().Before(deadline) {
				return fmt.Errorf("failed health gate: %s", lastReason)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(canaryPromoteHealthPoll):
			}
			continue
		}
		healthy, reason, err := c.checkContainerHealthy(ctx, *ct)
		if err != nil {
			return fmt.Errorf("health check: %w", err)
		}
		if healthy {
			break
		}
		lastReason = reason
		if !time.Now().Before(deadline) {
			return fmt.Errorf("failed health gate: %s", lastReason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(canaryPromoteHealthPoll):
		}
	}
	if wait := replaceSettleDelay - time.Since(started); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil
}

// replaceServiceRolling is replaceService's surge-of-one counterpart: for
// each replica it creates and starts the NEW container, waits for it to
// clear waitReplicaReady, THEN removes the one old container it's replacing
// — so live capacity never drops below the original replica count (it
// briefly rises to N+1) and services with proxy.unscalable: true never touch
// zero. On a mid-rollout failure it stops immediately and does NOT roll back
// replicas already swapped: surge-of-one never dropped capacity, so a
// partial state is still fully serving traffic. progress, if non-nil, is
// called once up front with the total replica count (done=0, so a status
// poll during the — often 5-35s — first replica's swap sees the real total
// instead of a zero value indistinguishable from "nothing planned"), then
// again after each replica is successfully swapped.
func (c *dockerClient) replaceServiceRolling(ctx context.Context, name string, req ReplaceServiceRequest, progress func(done, total int)) error {
	tpl, err := c.prepareReplaceTemplate(ctx, name, req)
	if err != nil {
		return err
	}

	total := len(tpl.tplSet)
	if progress != nil {
		progress(0, total)
	}
	replaced := map[string]bool{}
	for i, old := range tpl.tplSet {
		cname := fmt.Sprintf("goproxy-%s-%d", name, tpl.startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image:       req.Image,
			Labels:      tpl.newLabels,
			Env:         tpl.env,
			Healthcheck: tpl.clone.Healthcheck,
			HostConfig:  hostConfig{Mounts: tpl.clone.Mounts},
		})
		if err != nil {
			return fmt.Errorf("replaced %d/%d replicas, then failed on %s: create: %w", i, total, cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			_ = c.removeContainer(ctx, id)
			return fmt.Errorf("replaced %d/%d replicas, then failed on %s: start: %w", i, total, cname, err)
		}
		if err := c.waitReplicaReady(ctx, name, id); err != nil {
			return fmt.Errorf("replaced %d/%d replicas, then failed on %s: %w", i, total, cname, err)
		}

		_ = c.stopContainer(ctx, old.ID)
		if err := c.removeContainer(ctx, old.ID); err != nil {
			log.Printf("rolling-replace %s: failed to remove old %s: %v (new one is running)", name, old.name(), err)
		}
		replaced[old.ID] = true

		if progress != nil {
			progress(i+1, total)
		}
	}

	// Sweep any leftovers in existing that tplSet didn't pair with a
	// replacement (e.g. a stale exited container from a prior replace whose
	// removal silently failed) — replaceService's all-at-once tail removes
	// every member of existing unconditionally, and this rolling tail must
	// leave the same end state.
	for _, ct := range tpl.existing {
		if replaced[ct.ID] {
			continue
		}
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("rolling-replace %s: failed to remove old %s: %v (new ones are running)", name, ct.name(), err)
		}
	}
	return nil
}

// setAutoUpdateLabel flips proxy.autoupdate for a label-managed service via
// the same clone-and-recreate shape as replaceService, but WITHOUT an image
// swap: same image, same env, same mounts, only the label value changes.
// Lets the dashboard/MCP toggle unattended updates for any label-managed
// service without requiring a compose edit + `docker compose up -d`.
func (c *dockerClient) setAutoUpdateLabel(ctx context.Context, name string, enabled bool) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return fmt.Errorf("service %q not found (no live replicas)", name)
	}
	// See replaceService's identical tplSet comment: prefer a running
	// container as the template and as the recreate count, so a stale
	// exited leftover doesn't donate stale env/mounts or inflate the count.
	tplSet := preferRunning(existing)
	tpl := tplSet[0]

	env, err := c.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template env: %w", err)
	}
	clone, err := c.inspectCloneSpec(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template clone spec: %w", err)
	}

	newLabels := map[string]string{}
	for k, v := range tpl.Labels {
		if strings.HasPrefix(k, ociImageLabelPrefix) {
			continue
		}
		newLabels[k] = v
	}
	if enabled {
		newLabels[labelAutoUpdate] = "true"
	} else {
		delete(newLabels, labelAutoUpdate)
	}

	// startIdx still scans the full existing set — see replaceService's
	// identical comment on why naming must consider stale containers too.
	startIdx := nextReplicaIndex(existing, name)
	var newIDs []string
	for i := 0; i < len(tplSet); i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", name, startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image:       tpl.Image,
			Labels:      newLabels,
			Env:         env,
			Healthcheck: clone.Healthcheck,
			HostConfig:  hostConfig{Mounts: clone.Mounts},
		})
		if err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			_ = c.removeContainer(ctx, id)
			return fmt.Errorf("start %s: %w", cname, err)
		}
		newIDs = append(newIDs, id)
	}

	time.Sleep(replaceSettleDelay)

	for _, ct := range existing {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("autoupdate label flip %s: failed to remove old %s: %v (new ones are running)", name, ct.name(), err)
		}
	}
	return nil
}

// setUnscalableLabel flips proxy.unscalable for a label-managed service via
// the same clone-and-recreate shape as setAutoUpdateLabel, but WITHOUT an
// image swap: same image, same env, same mounts, only the label value
// changes. Lets the dashboard/MCP toggle the singleton flag for any
// label-managed service without requiring a compose edit + `docker compose
// up -d`. guardUnscalable is what enforces the "can't scale a singleton"
// constraint at scale-time; this setter's only job is to flip the label.
func (c *dockerClient) setUnscalableLabel(ctx context.Context, name string, enabled bool) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return fmt.Errorf("service %q not found (no live replicas)", name)
	}
	// See replaceService's identical tplSet comment: prefer a running
	// container as the template and as the recreate count, so a stale
	// exited leftover doesn't donate stale env/mounts or inflate the count.
	tplSet := preferRunning(existing)
	tpl := tplSet[0]

	env, err := c.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template env: %w", err)
	}
	clone, err := c.inspectCloneSpec(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template clone spec: %w", err)
	}

	newLabels := map[string]string{}
	for k, v := range tpl.Labels {
		if strings.HasPrefix(k, ociImageLabelPrefix) {
			continue
		}
		newLabels[k] = v
	}
	if enabled {
		newLabels[labelUnscalable] = "true"
	} else {
		delete(newLabels, labelUnscalable)
	}

	// startIdx still scans the full existing set — see replaceService's
	// identical comment on why naming must consider stale containers too.
	startIdx := nextReplicaIndex(existing, name)
	var newIDs []string
	for i := 0; i < len(tplSet); i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", name, startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image:       tpl.Image,
			Labels:      newLabels,
			Env:         env,
			Healthcheck: clone.Healthcheck,
			HostConfig:  hostConfig{Mounts: clone.Mounts},
		})
		if err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			_ = c.removeContainer(ctx, id)
			return fmt.Errorf("start %s: %w", cname, err)
		}
		newIDs = append(newIDs, id)
	}

	time.Sleep(replaceSettleDelay)

	for _, ct := range existing {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("unscalable label flip %s: failed to remove old %s: %v (new ones are running)", name, ct.name(), err)
		}
	}
	return nil
}

// parseWeightLabel reads a proxy.weight label the same way cmd/proxy's
// assembleGroups does — anything absent, unparseable, or non-positive means
// the default of 1. Kept deliberately identical so the number the dashboard
// shows is the number the proxy is actually routing on.
func parseWeightLabel(v string) int {
	if w, err := strconv.Atoi(v); err == nil && w > 0 {
		return w
	}
	return 1
}

// setWeightLabel sets proxy.weight on every replica of a label-managed
// service, via the same clone-and-recreate shape as setUnscalableLabel.
//
// Recreating is the whole cost here: Docker has no API to edit a label on a
// running container, so retuning a weight restarts the service's replicas.
// That is why the UI exposes this behind an explicit Apply rather than a
// stepper — see replicaCtrl's neighbour in ui.go.
func (c *dockerClient) setWeightLabel(ctx context.Context, name string, weight int) error {
	if weight < 1 {
		return fmt.Errorf("weight must be at least 1")
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	// Refuse mid-stage, which setUnscalableLabel does not need to do. Like
	// every label setter here this one only recreates the LIVE set, leaving
	// canary replicas on their stage-time label snapshot — and promoteCanary
	// later recreates those verbatim, so the new weight would be silently
	// reverted. Unscalable survives that because it is a boolean gate read
	// per-container; weight is arithmetic, summed across the whole group into
	// the one figure the peer is told, so a split set advertises a share that
	// matches neither the old value nor the requested one.
	if len(canaryOnly(all)) > 0 {
		return fmt.Errorf("service %q has a staged canary — promote or discard it before retuning the weight", name)
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return fmt.Errorf("service %q not found (no live replicas)", name)
	}
	// See replaceService's identical tplSet comment: prefer a running
	// container as the template and as the recreate count, so a stale
	// exited leftover doesn't donate stale env/mounts or inflate the count.
	tplSet := preferRunning(existing)
	tpl := tplSet[0]

	env, err := c.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template env: %w", err)
	}
	clone, err := c.inspectCloneSpec(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template clone spec: %w", err)
	}

	newLabels := map[string]string{}
	for k, v := range tpl.Labels {
		if strings.HasPrefix(k, ociImageLabelPrefix) {
			continue
		}
		newLabels[k] = v
	}
	// Weight 1 is the proxy's default, so drop the label entirely rather than
	// writing it out — that keeps a reset-to-default indistinguishable from a
	// service that never had a weight set.
	if weight == 1 {
		delete(newLabels, labelWeight)
	} else {
		newLabels[labelWeight] = strconv.Itoa(weight)
	}

	// startIdx still scans the full existing set — see replaceService's
	// identical comment on why naming must consider stale containers too.
	startIdx := nextReplicaIndex(existing, name)
	var newIDs []string
	for i := 0; i < len(tplSet); i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", name, startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image:       tpl.Image,
			Labels:      newLabels,
			Env:         env,
			Healthcheck: clone.Healthcheck,
			HostConfig:  hostConfig{Mounts: clone.Mounts},
		})
		if err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			_ = c.removeContainer(ctx, id)
			return fmt.Errorf("start %s: %w", cname, err)
		}
		newIDs = append(newIDs, id)
	}

	time.Sleep(replaceSettleDelay)

	for _, ct := range existing {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("weight label set %s: failed to remove old %s: %v (new ones are running)", name, ct.name(), err)
		}
	}
	return nil
}

// createCanaryReplicas creates `count` new canary replicas of a service,
// cloned from the current live template's env/mounts/labels but running
// req.Image — the shared primitive behind stageCanary (which always wants
// len(live), an immediate ~50/50 split) and a rollout's ramp steps (which
// want a smaller, growing count).
func (c *dockerClient) createCanaryReplicas(ctx context.Context, name string, req ReplaceServiceRequest, count int) error {
	if req.Image == "" {
		return fmt.Errorf("image is required")
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	if len(canaryOnly(all)) > 0 {
		return fmt.Errorf("%q already has a canary — promote or discard it first", name)
	}
	live := liveOnly(all)
	if len(live) == 0 {
		return fmt.Errorf("service %q has no live replicas", name)
	}
	// Prefer a running live container as the canary template — see
	// replaceService's identical tplSet comment.
	tpl := preferRunning(live)[0]

	base, err := c.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template env: %w", err)
	}
	clone, err := c.inspectCloneSpec(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect template clone spec: %w", err)
	}
	edits, refs, err := resolveSecretRefs(name, req.Env, c.secrets)
	if err != nil {
		return err
	}
	env, err := mergeEnv(base, edits, req.EnvAck)
	if err != nil {
		return redactRefConflicts(err, refs)
	}

	canaryLabels := map[string]string{}
	for k, v := range tpl.Labels {
		canaryLabels[k] = v
	}
	canaryLabels[labelCanary] = "true"
	canaryLabels[labelPrevImage] = tpl.Image

	c.pullImage(ctx, req.Image)
	startIdx := nextReplicaIndex(all, name)
	for i := 0; i < count; i++ {
		cname := fmt.Sprintf("goproxy-%s-canary-%d", name, startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image: req.Image, Labels: canaryLabels, Env: env, Healthcheck: clone.Healthcheck, HostConfig: hostConfig{Mounts: clone.Mounts},
		})
		if err != nil {
			return fmt.Errorf("create canary %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			return fmt.Errorf("start canary %s: %w", cname, err)
		}
	}
	return nil
}

// stageCanary creates additional replicas of a service with a new image. They
// share the live service's host/port labels, so the proxy round-robins traffic
// across BOTH live and canary while they coexist. No old containers removed.
func (c *dockerClient) stageCanary(ctx context.Context, name string, req ReplaceServiceRequest) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	// count prefers running live replicas — see replaceService's identical
	// tplSet comment — so a stale exited "live" leftover doesn't inflate the
	// canary pool created for the 50/50 split.
	return c.createCanaryReplicas(ctx, name, req, len(preferRunning(liveOnly(all))))
}

// nextCanaryReplicaIndex mirrors nextReplicaIndex but keys off the
// "goproxy-<service>-canary-" naming scheme — needed by scaleCanary so
// repeated ramp-step scale-ups (unlike stageCanary's single-shot creation)
// don't collide with canary names already in use.
func nextCanaryReplicaIndex(existing []dockerContainer, service string) int {
	max := 0
	prefix := "goproxy-" + service + "-canary-"
	for _, ct := range existing {
		n := ct.name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		if v, err := strconv.Atoi(n[len(prefix):]); err == nil && v > max {
			max = v
		}
	}
	return max + 1
}

// scaleCanary mirrors scaleService but operates on a service's canary
// replicas instead of its live ones — the primitive a rollout uses to grow
// or shrink the canary pool at each ramp step.
func (c *dockerClient) scaleCanary(ctx context.Context, name string, target int) error {
	if target < 0 {
		return fmt.Errorf("replicas must be >= 0")
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	existing := canaryOnly(all)
	if len(existing) == 0 {
		return fmt.Errorf("service %q has no canary replicas", name)
	}
	// Prefer a running canary as the template — see replaceService's
	// identical tplSet comment. current/target arithmetic below still counts
	// the full existing set: it drives the scale-down removal-count guard,
	// which operates over the same full set (mirrors scaleService).
	tpl := preferRunning(existing)[0]
	current := len(existing)
	switch {
	case current == target:
		return nil
	case current < target:
		env, err := c.inspectEnv(ctx, tpl.ID)
		if err != nil {
			return fmt.Errorf("inspect canary template %s: %w", tpl.name(), err)
		}
		clone, err := c.inspectCloneSpec(ctx, tpl.ID)
		if err != nil {
			return fmt.Errorf("inspect canary template %s: %w", tpl.name(), err)
		}
		startIdx := nextCanaryReplicaIndex(all, name)
		for i := 0; i < target-current; i++ {
			cname := fmt.Sprintf("goproxy-%s-canary-%d", name, startIdx+i)
			id, err := c.createContainer(ctx, cname, createBody{Image: tpl.Image, Labels: tpl.Labels, Env: env, Healthcheck: clone.Healthcheck, HostConfig: hostConfig{Mounts: clone.Mounts}})
			if err != nil {
				return fmt.Errorf("create canary %s: %w", cname, err)
			}
			if err := c.startContainer(ctx, id); err != nil {
				return fmt.Errorf("start canary %s: %w", cname, err)
			}
		}
	default:
		toRemove := current - target
		sort.Slice(existing, func(i, j int) bool { return existing[i].name() > existing[j].name() })
		for i := 0; i < toRemove; i++ {
			_ = c.stopContainer(ctx, existing[i].ID)
			if err := c.removeContainer(ctx, existing[i].ID); err != nil {
				return fmt.Errorf("remove %s: %w", existing[i].name(), err)
			}
		}
	}
	return nil
}

// promoteCanary tears down the live containers and removes the canary label
// from the canary ones — they become the new live.
func (c *dockerClient) promoteCanary(ctx context.Context, name string) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	canary := canaryOnly(all)
	live := liveOnly(all)
	if len(canary) == 0 {
		return fmt.Errorf("no canary to promote for %q", name)
	}
	// Health-gate BEFORE any recreate: once a canary container is recreated
	// without labelCanary below, it can no longer be found by anything that
	// looks up canary containers by label — including this same health
	// check and discardCanary-style rollback. Gating here, while the canary
	// set is still fully labeled, is what keeps a failed gate recoverable;
	// gating right before the old-live teardown (after the recreate loop)
	// would leave a failure in an unrecoverable state (see git history for
	// the bug this replaced).
	if err := c.waitForCanaryHealthy(ctx, name); err != nil {
		return fmt.Errorf("promote %s: %w", name, err)
	}
	// Recreate each canary container WITHOUT the canary label (Docker doesn't allow
	// label edits on running containers). Same env, same image, new name.
	for _, ct := range canary {
		env, err := c.inspectEnv(ctx, ct.ID)
		if err != nil {
			return fmt.Errorf("inspect canary env: %w", err)
		}
		clone, err := c.inspectCloneSpec(ctx, ct.ID)
		if err != nil {
			return fmt.Errorf("inspect canary clone spec: %w", err)
		}
		labels := map[string]string{}
		for k, v := range ct.Labels {
			if k == labelCanary {
				continue
			}
			labels[k] = v
		}
		startIdx := nextReplicaIndex(all, name)
		cname := fmt.Sprintf("goproxy-%s-%d", name, startIdx)
		id, err := c.createContainer(ctx, cname, createBody{Image: ct.Image, Labels: labels, Env: env, Healthcheck: clone.Healthcheck, HostConfig: hostConfig{Mounts: clone.Mounts}})
		if err != nil {
			return fmt.Errorf("create promoted %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			return fmt.Errorf("start promoted %s: %w", cname, err)
		}
		// Now safe to drop the original canary container.
		_ = c.stopContainer(ctx, ct.ID)
		_ = c.removeContainer(ctx, ct.ID)
		// Refresh the all list so nextReplicaIndex sees the new container.
		all = append(all, dockerContainer{ID: id, Names: []string{"/" + cname}})
	}
	// Tear down the old live.
	for _, ct := range live {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("promote %s: failed to remove old live %s: %v", name, ct.name(), err)
		}
	}
	return nil
}

// discardCanary removes the canary containers; live keeps serving unchanged.
func (c *dockerClient) discardCanary(ctx context.Context, name string) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	canary := canaryOnly(all)
	if len(canary) == 0 {
		return fmt.Errorf("no canary to discard for %q", name)
	}
	for _, ct := range canary {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			return err
		}
	}
	return nil
}

// deleteService permanently removes every container backing a label-managed
// service. It returns how many members were actually torn down (membersActed)
// alongside any error, so a partial failure partway through a multi-replica
// service can be reported accurately instead of as a bare status string —
// stopContainer is best-effort and deliberately NOT counted (only a
// successful removeContainer increments membersActed), since a stop with no
// matching remove leaves the container stopped but still present.
func (c *dockerClient) deleteService(ctx context.Context, name string) (membersActed int, err error) {
	existing, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return 0, err
	}
	for _, ct := range existing {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			return membersActed, err
		}
		membersActed++
	}
	return membersActed, nil
}

// ---- Routes view (independent of the proxy process) ----
//
// Reads container labels + the same routes.json file the proxy reads.
// Does NOT probe health — health is the proxy's job. Health column shows "unknown".

type RouteView struct {
	Host     string        `json:"host"`
	Path     string        `json:"path,omitempty"`
	Strip    bool          `json:"strip,omitempty"`
	Name     string        `json:"name,omitempty"`
	Service  string        `json:"service,omitempty"`
	Backends []BackendView `json:"backends"`
}
type BackendView struct {
	URL       string `json:"url"`
	Weight    int    `json:"weight"`
	Container string `json:"container,omitempty"`
}

type staticRoutesFile struct {
	Routes []struct {
		Host     string   `json:"host"`
		Path     string   `json:"path,omitempty"`
		Strip    bool     `json:"strip,omitempty"`
		Name     string   `json:"name,omitempty"`
		Backends []string `json:"backends"`
		Service  string   `json:"service,omitempty"`
	} `json:"routes"`
}

func (c *dockerClient) listRoutes(ctx context.Context, configPath string) ([]RouteView, error) {
	groups := map[string]*RouteView{}
	add := func(key string, fresh func() *RouteView) *RouteView {
		g, ok := groups[key]
		if !ok {
			g = fresh()
			groups[key] = g
		}
		return g
	}

	// staticKeys marks which host|path groups had their identity (and
	// Service field, if any) come from routes.json — used below to scope
	// Service-field backend backfill so a label-managed group's OWN
	// proxy.service label (already reflected in its Backends) isn't
	// mistaken for a resolution request and double-appended.
	staticKeys := map[string]bool{}

	// 1. Static config file.
	if configPath != "" {
		if data, err := os.ReadFile(configPath); err == nil {
			var cfg staticRoutesFile
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
			for _, sr := range cfg.Routes {
				key := sr.Host + "|" + sr.Path
				staticKeys[key] = true
				g := add(key, func() *RouteView {
					return &RouteView{Host: sr.Host, Path: sr.Path, Strip: sr.Strip, Name: sr.Name, Service: sr.Service}
				})
				for _, u := range sr.Backends {
					g.Backends = append(g.Backends, BackendView{URL: u, Weight: 1, Container: "static"})
				}
			}
		}
	}

	// backendsByService mirrors cmd/proxy/router.go's assembleGroups: one
	// BackendView per running, non-canary, proxy.service-labeled container,
	// keyed by service name — the pool a static route's Service field
	// backfills from below.
	backendsByService := map[string][]BackendView{}

	// 2. Docker labels.
	containers, err := c.listRunning(ctx, fmt.Sprintf(`{"label":["%s=true"]}`, labelEnable))
	if err != nil {
		return nil, err
	}
	for _, ct := range containers {
		host := ct.Labels[labelHost]
		portStr := ct.Labels[labelPort]
		if host == "" || portStr == "" {
			continue
		}
		if !validHostname(host) {
			log.Printf("route: skip container %s: invalid proxy.host label %q", ct.name(), host)
			continue
		}
		if svc := ct.Labels[labelService]; svc != "" && !validServiceName(svc) {
			log.Printf("route: skip container %s: invalid proxy.service label %q", ct.name(), svc)
			continue
		}
		if p := ct.Labels[labelPath]; p != "" && !validProxyPath(p) {
			log.Printf("route: skip container %s: invalid proxy.path label %q", ct.name(), p)
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		var ip string
		if n, ok := ct.NetworkSettings.Networks[managedNetwork]; ok && n.IPAddress != "" {
			ip = n.IPAddress
		} else {
			for _, n := range ct.NetworkSettings.Networks {
				if n.IPAddress != "" {
					ip = n.IPAddress
					break
				}
			}
		}
		if ip == "" {
			continue
		}
		path := ct.Labels[labelPath]
		g := add(host+"|"+path, func() *RouteView {
			return &RouteView{
				Host: host, Path: path, Strip: ct.Labels[labelStrip] == "true",
				Name: ct.Labels[labelName], Service: ct.Labels[labelService],
			}
		})
		weight := 1
		if w, err := strconv.Atoi(ct.Labels[labelWeight]); err == nil && w > 0 {
			weight = w
		}
		bv := BackendView{
			URL:       fmt.Sprintf("http://%s:%d", ip, port),
			Weight:    weight,
			Container: ct.name(),
		}
		g.Backends = append(g.Backends, bv)
		// Same canary exclusion as cmd/proxy/router.go: the motivating use
		// case is per-path rate limits on sensitive paths, where silently
		// routing to an in-progress canary is worse than the gap of
		// excluding it.
		if svc := ct.Labels[labelService]; svc != "" && ct.Labels[labelCanary] != "true" {
			backendsByService[svc] = append(backendsByService[svc], bv)
		}
	}

	for key := range staticKeys {
		g, ok := groups[key]
		if !ok || g.Service == "" {
			continue
		}
		if bs, ok := backendsByService[g.Service]; ok {
			g.Backends = append(g.Backends, bs...)
		} else {
			log.Printf("route: static route %s%s: service %q resolved to zero backends", g.Host, g.Path, g.Service)
		}
	}

	out := make([]RouteView, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.Backends, func(i, j int) bool { return g.Backends[i].URL < g.Backends[j].URL })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// serviceBackends reconstructs the upstream URLs the proxy would route to for
// a service: one per running non-canary replica, formatted exactly as
// cmd/proxy builds them ("http://%s:%d", ip, port).
//
// Canary replicas are included — they serve live traffic alongside the primary
// while staged, so their requests belong to this service too.
//
// Stopped members contribute nothing: they hold no IP and serve no traffic.
func serviceBackends(s *Service) []string {
	if s.Port == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range s.Members {
		if m.State != "running" {
			continue
		}
		for _, n := range m.NetworkSettings.Networks {
			if n.IPAddress == "" {
				continue
			}
			u := fmt.Sprintf("http://%s:%d", n.IPAddress, s.Port)
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	sort.Strings(out)
	return out
}
