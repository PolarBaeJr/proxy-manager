package main

import "regexp"

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
)

func validServiceName(s string) bool { return serviceNameRE.MatchString(s) }
func validHostname(s string) bool    { return hostnameRE.MatchString(s) }
func validProxyPath(s string) bool   { return proxyPathRE.MatchString(s) }
func validPort(p int) bool           { return p > 0 && p <= 65535 }

// validRoutePath additionally permits an empty string, which the router treats
// as a host-wide catch-all (equivalent to "/"). Any non-empty value must still
// be a well-formed proxy path (leading `/`, no metacharacters).
func validRoutePath(s string) bool { return s == "" || validProxyPath(s) }
