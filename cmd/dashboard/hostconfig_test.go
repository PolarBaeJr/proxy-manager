package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// hostConfigStub answers /containers/{id}/json with the given raw HostConfig
// JSON (nested under an envelope with Image/Config/NetworkSettings), and
// /images/{imageID}/json with the given raw image Config JSON. imageJSON may
// be empty when the test never expects the image-compare path to fire.
func hostConfigStub(t *testing.T, containerJSON, imageJSON string) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/images/"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(imageJSON))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(containerJSON))
		}
	}))
}

// TestInspectHostConfigUnknownsAllDefaults is a hand-written, fully
// zero-valued HostConfig — must pass with an empty refusal list.
func TestInspectHostConfigUnknownsAllDefaults(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:abc",
		"HostConfig": {
			"NetworkMode": "edge",
			"PortBindings": {},
			"Binds": null,
			"Dns": [], "DnsSearch": [], "DnsOptions": [],
			"ExtraHosts": null,
			"CapAdd": null, "CapDrop": null,
			"Privileged": false,
			"Devices": [],
			"Memory": 0, "MemorySwap": 0, "NanoCpus": 0, "CpuShares": 0,
			"CpusetCpus": "", "CpusetMems": "", "PidsLimit": 0
		},
		"Config": {"Cmd": null, "Entrypoint": null, "Healthcheck": null},
		"NetworkSettings": {"Networks": {"edge": {}}}
	}`, "")

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("refused = %v, want none", got)
	}
}

// TestInspectHostConfigUnknownsMountsOnlyPasses proves Mounts alone (Phase 0
// carries it forward) does not trigger a refusal.
func TestInspectHostConfigUnknownsMountsOnlyPasses(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:abc",
		"HostConfig": {
			"NetworkMode": "edge",
			"Mounts": [{"Type": "bind", "Source": "/host/data", "Target": "/app/data"}]
		},
		"Config": {},
		"NetworkSettings": {"Networks": {"edge": {}}}
	}`, "")

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("refused = %v, want none (Mounts is carried forward by Phase 0)", got)
	}
}

func TestInspectHostConfigUnknownsPortBindingsRefused(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:abc",
		"HostConfig": {
			"NetworkMode": "edge",
			"PortBindings": {"80/tcp": [{"HostPort": "8080"}]}
		},
		"Config": {},
		"NetworkSettings": {"Networks": {"edge": {}}}
	}`, "")

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if !contains(got, "PortBindings") {
		t.Fatalf("refused = %v, want PortBindings in the list", got)
	}
}

func TestInspectHostConfigUnknownsPrivilegedRefused(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:abc",
		"HostConfig": {
			"NetworkMode": "edge",
			"Privileged": true
		},
		"Config": {},
		"NetworkSettings": {"Networks": {"edge": {}}}
	}`, "")

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if !contains(got, "Privileged") {
		t.Fatalf("refused = %v, want Privileged in the list", got)
	}
}

// TestInspectHostConfigUnknownsRealisticContainerPasses is the discriminating
// test: a realistically COMPLETE docker inspect payload — Cmd populated (from
// the image, not a user override), MaskedPaths/ReadonlyPaths/LogConfig/
// ShmSize/Runtime/CgroupnsMode/IpcMode/ConsoleSize all populated the way the
// daemon always fills them in, Binds explicitly null, on the managed network
// only — must return an EMPTY refusal list. A hand-written all-zero fixture
// alone wouldn't catch a deny-by-default implementation that refuses on any
// populated key of a generic map decode.
func TestInspectHostConfigUnknownsRealisticContainerPasses(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:realimg",
		"HostConfig": {
			"NetworkMode": "edge",
			"RestartPolicy": {"Name": "unless-stopped"},
			"Binds": null,
			"PortBindings": {},
			"LogConfig": {"Type": "json-file", "Config": {}},
			"MaskedPaths": ["/proc/asound", "/proc/acpi", "/proc/kcore"],
			"ReadonlyPaths": ["/proc/bus", "/proc/fs", "/proc/irq"],
			"ShmSize": 67108864,
			"Runtime": "runc",
			"CgroupnsMode": "private",
			"IpcMode": "private",
			"ConsoleSize": [0, 0],
			"Memory": 0, "NanoCpus": 0, "CpuShares": 0
		},
		"Config": {
			"Cmd": ["nginx", "-g", "daemon off;"],
			"Entrypoint": null,
			"Healthcheck": null
		},
		"NetworkSettings": {"Networks": {"edge": {}}}
	}`, `{"Config": {"Cmd": ["nginx", "-g", "daemon off;"], "Entrypoint": null, "Healthcheck": null}}`)

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("refused = %v, want none — this is a realistic container with nothing actually overridden", got)
	}
}

// TestInspectHostConfigUnknownsCmdOverrideRefused proves the image-compare
// path actually catches a REAL override (not just image-inherited defaults).
func TestInspectHostConfigUnknownsCmdOverrideRefused(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:realimg",
		"HostConfig": {"NetworkMode": "edge"},
		"Config": {"Cmd": ["/bin/sh", "-c", "custom start script"]},
		"NetworkSettings": {"Networks": {"edge": {}}}
	}`, `{"Config": {"Cmd": ["nginx", "-g", "daemon off;"]}}`)

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if !contains(got, "Config.Cmd") {
		t.Fatalf("refused = %v, want Config.Cmd (differs from the image's own default)", got)
	}
}

// TestInspectHostConfigUnknownsCleanExtraNetworkPasses proves a container on
// a second (e.g. compose-project) network beyond the managed one is no
// longer refused, as long as that network carries nothing genuinely
// unreproducible — it's carried forward via cloneSpec/createContainer
// instead (see docker_test.go's end-to-end replaceService tests).
func TestInspectHostConfigUnknownsCleanExtraNetworkPasses(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:abc",
		"HostConfig": {"NetworkMode": "edge"},
		"Config": {},
		"NetworkSettings": {"Networks": {"edge": {}, "myproj_default": {}}}
	}`, "")

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("refused = %v, want none (a clean extra network is now carried forward)", got)
	}
}

// TestInspectHostConfigUnknownsStaticIPNetworkRefused proves an extra network
// whose endpoint carries a static IP (IPAMConfig) is still refused — a
// recreate has no way to reproduce it.
func TestInspectHostConfigUnknownsStaticIPNetworkRefused(t *testing.T) {
	dc := hostConfigStub(t, `{
		"Image": "sha256:abc",
		"HostConfig": {"NetworkMode": "edge"},
		"Config": {},
		"NetworkSettings": {"Networks": {"edge": {}, "myproj_default": {"IPAMConfig": {"IPv4Address": "172.20.0.5"}}}}
	}`, "")

	got, err := dc.inspectHostConfigUnknowns(context.Background(), "id1")
	if err != nil {
		t.Fatalf("inspectHostConfigUnknowns: %v", err)
	}
	if !contains(got, "NetworkSettings.Networks.myproj_default.IPAMConfig") {
		t.Fatalf("refused = %v, want the static-IP network flagged", got)
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
