// Secret-by-reference env values.
//
// Env edits (dashboard "Env override" box, MCP replace_service/stage_canary
// env param) normally carry literal values, which means a real secret typed
// there lands in the caller's transcript — an MCP tool-call argument is not a
// safe place for one. A value of the form "ref:NAME" is instead resolved
// server-side, at replace/stage time, by reading NAME out of a file bind-
// mounted read-only into this container (SECRETS_FILE, never touched over
// the API). Only the ref survives in request bodies and persisted onboarded
// records; the resolved value exists in memory just long enough to reach the
// Docker container-create call.
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
	"strings"
)

const (
	secretRefPrefix    = "ref:"
	defaultSecretsFile = "/etc/secrets.env"
)

// secretsStore is the bind-mounted secrets file. Read fresh on every lookup
// rather than cached: replace/stage is infrequent, and this means a rotated
// secret is picked up on the very next deploy with no dashboard restart.
type secretsStore struct {
	path string
}

// newSecretsFromEnv resolves SECRETS_FILE (default /etc/secrets.env). Returns
// nil when the file is absent or isn't a regular file, along with lines the
// caller should log.
//
// Deliberately does NOT create the file: on a host without the bind mount,
// the ref: feature would otherwise report itself "configured" while nothing
// backs it.
func newSecretsFromEnv(getenv func(string) string) (*secretsStore, []string) {
	path := strings.TrimSpace(getenv("SECRETS_FILE"))
	if path == "" {
		path = defaultSecretsFile
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, []string{"⚠ secret refs (ref:NAME env values) disabled: " + path + " not available: " + err.Error()}
	}
	if st.IsDir() {
		return nil, []string{"⚠ secret refs (ref:NAME env values) disabled: " + path + " is a directory, not a file"}
	}
	return &secretsStore{path: path}, nil
}

func (s *secretsStore) lookup(name string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("secret %q requested but no secrets file is configured (set SECRETS_FILE)", name)
	}
	f, err := os.Open(s.path)
	if err != nil {
		return "", fmt.Errorf("read secrets file: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := splitEnvEntry(line); ok && k == name {
			return v, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read secrets file: %w", err)
	}
	return "", fmt.Errorf("secret %q not found in secrets file", name)
}

// resolveSecretRefs returns edits with every "ref:NAME" value resolved to the
// actual secret; literal values pass through unchanged. edits itself is never
// mutated. The second return maps each ref-sourced key back to its original
// "ref:NAME" string — callers must use it to redact envConflictError before
// it reaches an API response (see redactRefConflicts), otherwise a resolved
// secret leaks into the conflict picker. A resolution failure names the env
// key, never the secret name's value, and stops the request before any
// container is created.
func resolveSecretRefs(edits map[string]string, sec *secretsStore) (resolved map[string]string, refs map[string]string, err error) {
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
		val, lookupErr := sec.lookup(name)
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
