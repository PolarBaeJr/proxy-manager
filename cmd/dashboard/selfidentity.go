// selfidentity: lets the dashboard tell its OWN container apart from every
// other container it manages, so /api/services (and the mutating handler
// under /api/services/{name}/...) can refuse to list or act on itself —
// stopping or replacing the dashboard from within its own UI would kill the
// operator's own session with no way back in short of shelling into the
// host.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// selfHostname is os.Hostname, overridable in tests — same seam convention as
// docker.go's replaceSettleDelay.
var selfHostname = os.Hostname

// isSelfContainer reports whether ct is this dashboard process's own
// container. Docker defaults an unnamed container's hostname to its own
// short container ID (see docker-compose.yml's dashboard service — no
// hostname: override), so os.Hostname() inside the process returns a prefix
// of ct.ID for the dashboard's own container and nothing else.
func isSelfContainer(ct dockerContainer) bool {
	hostname, err := selfHostname()
	if err != nil {
		return false
	}
	h := strings.TrimSpace(hostname)
	if h == "" {
		return false
	}
	return ct.ID == h || strings.HasPrefix(ct.ID, h)
}

// serviceContainsSelf reports whether any member container of svc is this
// dashboard's own container.
func serviceContainsSelf(svc Service) bool {
	for _, m := range svc.Members {
		if isSelfContainer(m) {
			return true
		}
	}
	return false
}

// excludeSelf filters the dashboard's own service out of a managed-services
// list — used by /api/services so the dashboard never lists itself as
// something you can stop/replace/scale from within its own UI. Filters by
// container identity (this process's actual container ID), never by name —
// a different, unrelated container that happens to be named "dashboard" is
// NOT excluded.
func excludeSelf(svcs []Service) []Service {
	out := make([]Service, 0, len(svcs))
	for _, s := range svcs {
		if serviceContainsSelf(s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// serviceContainsSelfByName reports whether the named service (as currently
// discovered live from Docker) is this dashboard's own container — used by
// the mutating /api/services/{name}/... handler to refuse any action
// targeting the dashboard's own service, independent of whatever list the
// UI last rendered.
func (c *dockerClient) serviceContainsSelfByName(ctx context.Context, name string) (bool, error) {
	containers, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return false, err
	}
	for _, ct := range containers {
		if isSelfContainer(ct) {
			return true, nil
		}
	}
	return false, nil
}
