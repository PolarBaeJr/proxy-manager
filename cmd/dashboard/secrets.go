// Secret-by-reference env values.
//
// Env edits (dashboard "Env override" box, MCP replace_service/stage_canary
// env param) normally carry literal values, which means a real secret typed
// there lands in the caller's transcript — an MCP tool-call argument is not a
// safe place for one. A value of the form "ref:NAME" is instead resolved
// server-side, at replace/stage time, by reading NAME out of a per-SERVICE
// file under a directory bind-mounted read-only into this container
// (SECRETS_DIR, never touched over the API). Only the ref survives in
// request bodies and persisted onboarded records; the resolved value exists
// in memory just long enough to reach the Docker container-create call.
//
// One file per service (<dir>/<service>.env) rather than one shared file:
// with several unrelated services on the same host, a flat namespace means
// two services picking the same key name (API_KEY, WEBHOOK_SECRET, ...)
// silently share a value, or a KEY_ROTATED_LATER shadows an unrelated
// service's key of the same name — a naming-convention footgun, not a
// structural guarantee. Scoping by service makes collisions across services
// impossible rather than merely discouraged.
//
// Mirrors maintenance.go's pattern: a nil *secretsStore means the feature
// isn't configured, and every ref: lookup degrades to a clear error instead
// of the dashboard silently pretending secrets are available.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	secretRefPrefix   = "ref:"
	defaultSecretsDir = "/etc/secrets"
)

// secretsStore is the bind-mounted secrets directory, one file per service.
// Read fresh on every lookup rather than cached: replace/stage is
// infrequent, and this means a rotated secret is picked up on the very next
// deploy with no dashboard restart.
type secretsStore struct {
	dir string
}

// newSecretsFromEnv resolves SECRETS_DIR (default /etc/secrets). Returns nil
// when the directory is absent or isn't actually a directory, along with
// lines the caller should log.
//
// Deliberately does NOT create the directory: on a host without the bind
// mount, the ref: feature would otherwise report itself "configured" while
// nothing backs it.
func newSecretsFromEnv(getenv func(string) string) (*secretsStore, []string) {
	dir := strings.TrimSpace(getenv("SECRETS_DIR"))
	if dir == "" {
		dir = defaultSecretsDir
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, []string{"⚠ secret refs (ref:NAME env values) disabled: " + dir + " not available: " + err.Error()}
	}
	if !st.IsDir() {
		return nil, []string{"⚠ secret refs (ref:NAME env values) disabled: " + dir + " is a file, not a directory"}
	}
	return &secretsStore{dir: dir}, nil
}

// servicePath is the ONLY place a caller-supplied service name becomes a
// filesystem path. validServiceName rejects anything that could traverse
// (no "/", and it can't start with "." so a bare ".." never matches), same
// allowlist already trusted to name a Docker container.
func (s *secretsStore) servicePath(service string) (string, error) {
	if !validServiceName(service) {
		return "", fmt.Errorf("invalid service name %q", service)
	}
	return filepath.Join(s.dir, service+".env"), nil
}

func (s *secretsStore) lookup(service, name string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("secret %q requested but no secrets directory is configured (set SECRETS_DIR)", name)
	}
	path, err := s.servicePath(service)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no secrets file for service %q (expected %s)", service, path)
		}
		return "", fmt.Errorf("read secrets file: %w", err)
	}
	defer f.Close()

	found := false
	var val string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Keep scanning past a match rather than returning immediately: if
		// the key repeats (e.g. a secret rotated by appending a new line
		// instead of editing in place), the LAST occurrence wins — same
		// convention mergeEnv already uses for a container's own env, so a
		// duplicate behaves the same way in both places.
		if k, v, ok := splitEnvEntry(line); ok && k == name {
			found, val = true, v
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read secrets file: %w", err)
	}
	if !found {
		return "", fmt.Errorf("secret %q not found in %s's secrets file", name, service)
	}
	return val, nil
}

// resolveSecretRefs returns edits with every "ref:NAME" value resolved to the
// actual secret, scoped to service's own secrets file; literal values pass
// through unchanged. edits itself is never mutated. The second return maps
// each ref-sourced key back to its original "ref:NAME" string — callers must
// use it to redact envConflictError before it reaches an API response (see
// redactRefConflicts), otherwise a resolved secret leaks into the conflict
// picker. A resolution failure names the env key, never the secret name's
// value, and stops the request before any container is created.
func resolveSecretRefs(service string, edits map[string]string, sec *secretsStore) (resolved map[string]string, refs map[string]string, err error) {
	if len(edits) == 0 {
		return edits, nil, nil
	}
	resolved = make(map[string]string, len(edits))
	for k, v := range edits {
		name, isRef := strings.CutPrefix(v, secretRefPrefix)
		if !isRef {
			resolved[k] = v
			continue
		}
		val, lookupErr := sec.lookup(service, name)
		if lookupErr != nil {
			return nil, nil, fmt.Errorf("env %s: %w", k, lookupErr)
		}
		resolved[k] = val
		if refs == nil {
			refs = map[string]string{}
		}
		refs[k] = v
	}
	return resolved, refs, nil
}

// redactRefConflicts rewrites an envConflictError so a ref-sourced key's
// Incoming field shows the original "ref:NAME" instead of the secret value
// mergeEnv compared against the current env. Every other error passes
// through unchanged.
func redactRefConflicts(err error, refs map[string]string) error {
	var ce *envConflictError
	if !errors.As(err, &ce) || len(refs) == 0 {
		return err
	}
	out := make([]EnvConflict, len(ce.Conflicts))
	for i, c := range ce.Conflicts {
		if ref, isRef := refs[c.Key]; isRef {
			c.Incoming = ref
		}
		out[i] = c
	}
	return &envConflictError{Conflicts: out}
}
