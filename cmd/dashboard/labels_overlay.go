// Central labels, dashboard read side: the dashboard's own decisions
// (scaling guard, drain grace, overlap, health gate, autoupdate, the
// services list) read an adopted service's managed keys from the central
// labels snapshot, exactly as the proxy overlays them. Create/clone paths
// and adopt strictness keep reading RAW container labels: overlay values are
// never baked into a container.
package main

import (
	"context"
	"errors"
)

// effectiveLabels returns a copy of raw with its service's managed keys
// replaced by the central labels when the service (raw's proxy.service) is
// adopted: present → set, absent → deleted. Always a copy — raw may be the
// container cache's shared map.
func (c *dockerClient) effectiveLabels(raw map[string]string) map[string]string {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	m, ok := c.adoptedLabels(raw[labelService])
	if !ok {
		return out
	}
	for _, k := range managedLabelKeys {
		if v, ok := m[k]; ok {
			out[k] = v
		} else {
			delete(out, k)
		}
	}
	return out
}

// adoptedLabels is svc's central map from the snapshot. Nil-safe: no store
// (LABELS_CENTRAL off) or no snapshot yet means not adopted.
func (c *dockerClient) adoptedLabels(svc string) (map[string]string, bool) {
	if c == nil || c.labels == nil || svc == "" {
		return nil, false
	}
	snap := c.labels.Snapshot()
	if snap == nil {
		return nil, false
	}
	m, ok := snap.Services[svc]
	return m, ok
}

// labelsAdopted reports whether svc's labels are centrally managed, per the
// snapshot.
func (c *dockerClient) labelsAdopted(svc string) bool {
	_, ok := c.adoptedLabels(svc)
	return ok
}

// labelsUnknown: central labels are on here but have never loaded, so
// whether any service is adopted is unknown.
func (c *dockerClient) labelsUnknown() bool {
	return c != nil && c.labels != nil && !c.labels.EverLoaded()
}

// effectiveDrainSeconds is drainSeconds over the effective labels.
func (c *dockerClient) effectiveDrainSeconds(raw map[string]string) int {
	return drainSeconds(c.effectiveLabels(raw))
}

// redirectLabelSetter turns a recreate-based label setter into a central
// labels write when svc is adopted — recreating would only rewrite container
// labels the overlay ignores. handled=false means "not adopted, recreate as
// before". Once adopted it never falls back to recreating, not even with
// Redis down or writes disabled. The default values (false, weight 1)
// become an unset, matching what the setters do to container labels.
func (c *dockerClient) redirectLabelSetter(ctx context.Context, svc, setter, key, value string) (bool, error) {
	if !c.labelsAdopted(svc) {
		return false, nil
	}
	req := labelsSetRequest{}
	if (key == labelWeight && value == "1") || value == "false" {
		req.Unset = []string{key}
	} else {
		req.Set = map[string]string{key: value}
	}
	for attempt := 0; ; attempt++ {
		cur, err := c.currentLabelsVersion(ctx, svc)
		if err != nil {
			return true, err
		}
		req.IfVersion = cur
		_, err = commitLabelsChange(ctx, c, nil, svc, req, nil, "dashboard", setter)
		if errors.Is(err, errLabelsNotAdopted) {
			// Released since the snapshot was taken: Redis is authoritative.
			return false, nil
		}
		var conflict errLabelsVersionConflict
		if errors.As(err, &conflict) && attempt == 0 {
			continue
		}
		return true, err
	}
}

// currentLabelsVersion reads svc's version fresh from Redis (0 when not
// adopted).
func (c *dockerClient) currentLabelsVersion(ctx context.Context, svc string) (uint64, error) {
	if c.labels == nil {
		return 0, errLabelsDisabled
	}
	rec, _, err := c.labels.Get(ctx, svc)
	if err != nil {
		return 0, errLabelsUnavailable
	}
	return rec.Version, nil
}
