// Env resolution for Replace / Stage.
//
// The old rule was all-or-nothing: a request carrying ANY env replaced the
// service's entire environment, so typing one line in the dashboard's "Env
// override" box silently dropped every other variable the container was
// running. Now the request carries *edits*, which are merged onto what the
// service is actually running:
//
//	key absent from current env      -> appended
//	key present, same value          -> no-op
//	key present, different value     -> CONFLICT, caller must choose
//
// A conflict is not resolved server-side. mergeEnv reports every conflicting
// key at once (one round-trip, not one per variable) and the caller re-submits
// with the chosen value plus the key in EnvAck to say "yes, overwrite it".
//
// Note this pins variables that come from the image: Docker merges the image's
// own ENV into Config.Env, so a key like PORT baked into the image counts as
// "current" and will prompt. That is deliberate — the alternative is silently
// preferring one source over the other — but it does mean the dialog can ask
// about a variable the user never set by hand.
package main

import (
	"sort"
	"strings"
)

// EnvConflict is one variable whose edited value disagrees with what the
// service is currently running. Serialized to the browser in the 409 body.
type EnvConflict struct {
	Key      string `json:"key"`
	Current  string `json:"current"`
	Incoming string `json:"incoming"`
}

// envConflictError carries every unresolved conflict from a single merge.
// The API layer turns it into a 409 the dashboard can render as a picker.
type envConflictError struct {
	Conflicts []EnvConflict
}

func (e *envConflictError) Error() string {
	keys := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		keys = append(keys, c.Key)
	}
	return "env conflict on " + strings.Join(keys, ", ") + ": choose which value to keep"
}

// splitEnvEntry splits a "KEY=VALUE" entry. An entry with no "=" is not a
// variable assignment (Docker passes it through as an inherit-from-daemon
// request) and is reported as not splittable so it's left strictly alone.
func splitEnvEntry(s string) (key, val string, ok bool) {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// mergeEnv overlays edits onto base, returning the env for the new containers.
//
// With no edits it returns base *itself*, unchanged and unreordered. The
// unattended auto-updater replaces services with no env edits at all, so any
// normalization here would silently rewrite the environment of every
// auto-updating service on each release.
func mergeEnv(base []string, edits map[string]string, ack []string) ([]string, error) {
	if len(edits) == 0 {
		return base, nil
	}

	acked := make(map[string]bool, len(ack))
	for _, k := range ack {
		acked[k] = true
	}

	out := make([]string, len(base))
	copy(out, base)

	// Index by key. Docker keeps the LAST occurrence when a key repeats, so
	// index the last one — that's the value the container is actually running
	// and therefore the one to compare against and overwrite.
	at := make(map[string]int, len(base))
	for i, e := range base {
		if k, _, ok := splitEnvEntry(e); ok {
			at[k] = i
		}
	}

	var conflicts []EnvConflict
	var added []string
	for k, v := range edits {
		i, exists := at[k]
		if !exists {
			added = append(added, k+"="+v)
			continue
		}
		_, cur, _ := splitEnvEntry(out[i])
		switch {
		case cur == v:
			// Same value — nothing to ask about and nothing to change.
		case acked[k]:
			out[i] = k + "=" + v
		default:
			conflicts = append(conflicts, EnvConflict{Key: k, Current: cur, Incoming: v})
		}
	}

	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Key < conflicts[j].Key })
		return nil, &envConflictError{Conflicts: conflicts}
	}

	// Sorted so the same request always produces the same env — map iteration
	// order would otherwise make the result differ run to run.
	sort.Strings(added)
	return append(out, added...), nil
}
