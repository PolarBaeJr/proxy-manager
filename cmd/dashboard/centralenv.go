// Central per-service env: the resolver every create path consults.
//
// Behind the global CENTRAL_ENV flag (default off). With it off,
// dockerClient.central stays nil and every create path clones env from a
// local template container exactly as before. With it on, a service is
// "centrally managed" when this host's origin store owns it, when this
// host's cache holds an env for it, or when its existing replicas carry
// pmgr.env.origin — and then its replicas are created ONLY from the central
// env. A managed service whose central env can't be produced is refused,
// never quietly created from local env instead.
package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// labelEnvOrigin / labelEnvVersion are stamped on every replica created
	// from a central env: which dashboard owns the env, and which version
	// the replica was created from.
	labelEnvOrigin  = "pmgr.env.origin"
	labelEnvVersion = "pmgr.env.version"

	composeLabelPrefix = "com.docker.compose."

	// centralEnvFeature is the peer-handshake capability string a dashboard
	// advertises when CENTRAL_ENV is on.
	centralEnvFeature = "central-env/1"
)

// centralEnvResult is a resolved env for one replica on this host. Warning is
// set when Stale is (origin unreachable, cached copy used) and names only the
// service, origin, version and fetch time — never a value.
type centralEnvResult struct {
	Env     []string
	Version uint64
	Origin  string
	Stale   bool
	Warning string
}

// centralEnvResolver is what dockerClient's create paths and the spread
// flow depend on. Resolve is the per-create lookup; Managed is its cheap,
// network-free twin for paths that only need to refuse; ResolveForPeer and
// Accept are the two ends of shipping a central env to a spread target.
type centralEnvResolver interface {
	Enabled() bool
	Resolve(ctx context.Context, svc string, existingLabels map[string]string) (centralEnvResult, bool, error)
	Managed(svc string, existingLabels map[string]string) bool
	ResolveForPeer(svc, identity string) (centralEnvResult, bool, error)
	Accept(svc, origin string, version uint64, env []string) error
}

// errCentralEnvUnavailable: svc is centrally managed from origin, but this
// host has neither a reachable origin nor a cached copy. Every create is
// refused until one of those is true.
type errCentralEnvUnavailable struct {
	Service string
	Origin  string
}

func (e errCentralEnvUnavailable) Error() string {
	return fmt.Sprintf("central env for %q is unavailable (origin %s unreachable and nothing cached) — refusing to create replicas from local env", e.Service, e.Origin)
}

// errEnvCentrallyManaged refuses an operation that would bypass a service's
// central env — per-request env edits, or a path (duplicate, onboarded) that
// has no way to honor it.
type errEnvCentrallyManaged struct {
	Service string
	Hint    string
}

func (e errEnvCentrallyManaged) Error() string {
	return fmt.Sprintf("%q is centrally managed — %s", e.Service, e.Hint)
}

type centralEnv struct {
	enabled  bool
	identity string
	store    *centralEnvStore
	cache    *centralEnvCache
	secrets  *secretsStore
	// fetchFromOrigin asks the origin dashboard for svc's env as resolved
	// for forIdentity. Nil (PR-A: not wired yet) behaves exactly like an
	// unreachable origin.
	fetchFromOrigin func(ctx context.Context, origin, svc, forIdentity string) (env []string, version uint64, err error)
}

func (ce *centralEnv) Enabled() bool { return ce != nil && ce.enabled }

func (ce *centralEnv) Managed(svc string, existingLabels map[string]string) bool {
	if !ce.Enabled() {
		return false
	}
	if ce.store.Has(svc) || existingLabels[labelEnvOrigin] != "" {
		return true
	}
	_, ok := ce.cache.Get(svc)
	return ok
}

func (ce *centralEnv) Resolve(ctx context.Context, svc string, existingLabels map[string]string) (centralEnvResult, bool, error) {
	if !ce.Enabled() {
		return centralEnvResult{}, false, nil
	}
	if ce.store.Has(svc) {
		env, version, err := ce.store.effectiveEnvAt(svc, ce.identity, ce.secrets)
		if err != nil {
			return centralEnvResult{}, true, err
		}
		return centralEnvResult{Env: env, Version: version, Origin: ce.identity}, true, nil
	}

	cached, hasCache := ce.cache.Get(svc)
	origin := existingLabels[labelEnvOrigin]
	if origin == "" && hasCache {
		origin = cached.Origin
	}
	if origin == "" {
		return centralEnvResult{}, false, nil
	}
	if origin == ce.identity {
		// Replicas claim this host owns the env, but the store has no
		// record. Fail closed rather than fall back to the template's local
		// env — the record may have been lost, not deliberately removed.
		return centralEnvResult{}, true, errCentralEnvUnavailable{Service: svc, Origin: origin}
	}
	if hasCache && cached.Origin != origin {
		// A cache from a different origin says nothing about this one.
		hasCache = false
	}

	if ce.fetchFromOrigin != nil {
		env, version, err := ce.fetchFromOrigin(ctx, origin, svc, ce.identity)
		if err == nil {
			putErr := ce.cache.Put(svc, origin, version, env)
			var older errCentralEnvCacheOlder
			switch {
			case putErr == nil:
				return centralEnvResult{Env: env, Version: version, Origin: origin}, true, nil
			case !errors.As(putErr, &older):
				return centralEnvResult{Env: env, Version: version, Origin: origin,
					Warning: fmt.Sprintf("central env for %q: fetched version %d from %s but could not cache it", svc, version, origin)}, true, nil
			}
			// An origin answering with an older version than we already
			// hold falls through to the cached copy below.
		}
	}
	if hasCache {
		return centralEnvResult{
			Env: cached.Env, Version: cached.Version, Origin: origin, Stale: true,
			Warning: fmt.Sprintf("central env for %q: origin %s unreachable — using cached version %d fetched %s",
				svc, origin, cached.Version, time.Unix(cached.FetchedAt, 0).UTC().Format(time.RFC3339)),
		}, true, nil
	}
	return centralEnvResult{}, true, errCentralEnvUnavailable{Service: svc, Origin: origin}
}

// ResolveForPeer is Resolve for a replica on ANOTHER host: svc's env with
// identity's overrides applied. Only the origin can answer that (a cache
// holds only this host's own resolution), so owned=false means "not this
// host's to ship".
func (ce *centralEnv) ResolveForPeer(svc, identity string) (centralEnvResult, bool, error) {
	if !ce.Enabled() || !ce.store.Has(svc) {
		return centralEnvResult{}, false, nil
	}
	env, version, err := ce.store.effectiveEnvAt(svc, identity, ce.secrets)
	if err != nil {
		return centralEnvResult{}, true, err
	}
	return centralEnvResult{Env: env, Version: version, Origin: ce.identity}, true, nil
}

// Accept caches an env the origin shipped to this host (spread's seed). It
// refuses for a service this host is itself the origin of — a peer must
// never overwrite the authoritative copy through the cache.
func (ce *centralEnv) Accept(svc, origin string, version uint64, env []string) error {
	if !ce.Enabled() {
		return fmt.Errorf("central env is not enabled on this host")
	}
	if origin == ce.identity || ce.store.Has(svc) {
		return fmt.Errorf("this host is the central env origin for %q — refusing a peer-supplied copy", svc)
	}
	return ce.cache.Put(svc, origin, version, env)
}

// stampEnvLabels returns a COPY of src with the central-env provenance
// labels set and every com.docker.compose.* label dropped. Always a copy:
// src is often a container's Labels map straight out of the shared list
// cache (see dockerContainer.Labels), which nothing may write to. Compose
// labels go because a replica built from central env is no longer what the
// compose file describes — leaving them would let `docker compose` treat it
// as its own and recreate or remove it.
func stampEnvLabels(src map[string]string, res centralEnvResult) map[string]string {
	out := make(map[string]string, len(src)+2)
	for k, v := range src {
		if strings.HasPrefix(k, composeLabelPrefix) {
			continue
		}
		out[k] = v
	}
	out[labelEnvOrigin] = res.Origin
	out[labelEnvVersion] = strconv.FormatUint(res.Version, 10)
	return out
}

// subtractImageEnv returns the part of a container's env that the container
// itself set: a key is dropped only when the image's own env has the exact
// same value (i.e. the container merely inherited it). A container that
// overrides an image default keeps its value.
func subtractImageEnv(containerEnv, imageEnv []string) map[string]string {
	img := make(map[string]string, len(imageEnv))
	for _, e := range imageEnv {
		if k, v, ok := splitEnvEntry(e); ok {
			img[k] = v
		}
	}
	out := map[string]string{}
	for _, e := range containerEnv {
		k, v, ok := splitEnvEntry(e)
		if !ok {
			continue
		}
		if iv, inImage := img[k]; inImage && iv == v {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}
