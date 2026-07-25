package main

import (
	"regexp"
	"strings"
)

// Strict allowlists for values that end up interpolated into HTML/JS on the
// dashboard. Every regex rejects the entire universe of HTML/JS metacharacters
// (' " < > \ / space etc), so a rendered value cannot break out of a JS
// string literal or an HTML attribute — even if the render path forgets to
// escape it. Applied at BOTH ingestion points:
//   - user-submitted values on create/onboard endpoints (server-side)
//   - proxy.* labels read from the docker socket (rejects rogue-image XSS)
// The frontend also enforces the same shapes via HTML5 `pattern=` attributes
// for immediate feedback, but the backend is the security boundary.

var (
	// Docker service/container name shape. First char alnum, rest alnum + . _ -.
	// Capped at 63 chars (docker's own limit).
	serviceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

	// DNS-shaped hostname. Alnum, dots, hyphens only — no path, query,
	// port, protocol, or any HTML/JS metacharacter.
	hostnameRE = regexp.MustCompile(`^[a-zA-Z0-9.-]{1,253}$`)

	// URL path prefix used for label-based routing. Must start with `/`,
	// alnum + / _ . - only. No query, no fragment, no encoded characters.
	proxyPathRE = regexp.MustCompile(`^/[A-Za-z0-9/_.-]{0,511}$`)

	// Cloudflare DNS record id — a hex-ish opaque token that gets appended to
	// the Cloudflare API URL, so nothing path-ish may pass.
	cfRecordIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

	// FILENAME allowlist for maintenance flag files. Deliberately stricter
	// than hostnameRE, which admits ".", "..", "..." and "-x" — and
	// filepath.Join(dir, "..") escapes the directory. A hostname validator is
	// NOT a filename validator, so maintenance paths use this one instead.
	// Lowercase only: nginx matches these files against $host, which it
	// lowercases, so an uppercase flag file would be a silent no-op — callers
	// must normalize before validating. Must start AND end alnum. Max 253.
	maintHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

func validServiceName(s string) bool { return serviceNameRE.MatchString(s) }
func validHostname(s string) bool    { return hostnameRE.MatchString(s) }
func validProxyPath(s string) bool   { return proxyPathRE.MatchString(s) }
func validCFRecordID(s string) bool  { return cfRecordIDRE.MatchString(s) }
func validPort(p int) bool           { return p > 0 && p <= 65535 }

// validMaintHost gates the only value that ever becomes a maintenance flag
// filename. The regex's leading/trailing-alnum anchors already reject ".",
// "..", "..." and "../x"; the Contains check adds embedded "..", which the
// regex DOES admit (e.g. "a..b"). This is a path-traversal boundary.
func validMaintHost(s string) bool {
	return maintHostRE.MatchString(s) && !strings.Contains(s, "..")
}

// validRoutePath additionally permits an empty string, which the router treats
// as a host-wide catch-all (equivalent to "/"). Any non-empty value must still
// be a well-formed proxy path (leading `/`, no metacharacters).
func validRoutePath(s string) bool { return s == "" || validProxyPath(s) }
