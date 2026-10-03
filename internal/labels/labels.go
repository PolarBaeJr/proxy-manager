// Package labels is the registry of proxy.* container labels that can be
// managed centrally in Redis (docs/CENTRAL_LABELS_PLAN.md): which keys are
// live-tunable, how each value is validated, and the Redis key names. Shared
// by cmd/proxy (read-only overlay) and cmd/dashboard (writes); must never
// import cmd/*.
package labels

import (
	"fmt"
	"net/textproto"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Class says who consumes a key and whether it may be set via Redis.
type Class int

const (
	// ProxyLive keys are read by the proxy router on every refresh.
	ProxyLive Class = iota + 1
	// DashboardLive keys are read by the dashboard (lifecycle, autoupdate, UI).
	DashboardLive
	// Security keys stay container-only: a Redis credential must not be
	// enough to switch auth off.
	Security
	// Identity keys define routing identity; changing them is a recreate.
	Identity
	// PerReplica keys are written by the dashboard on individual replicas.
	PerReplica
)

func (c Class) String() string {
	switch c {
	case ProxyLive:
		return "proxy-live"
	case DashboardLive:
		return "dashboard-live"
	case Security:
		return "security"
	case Identity:
		return "identity"
	case PerReplica:
		return "per-replica"
	}
	return "unknown"
}

// Spec describes one registry key. Validate is nil for keys that can never be
// set through Redis.
type Spec struct {
	Key      string
	Class    Class
	Validate func(string) error
	Consumer string
}

const (
	Weight      = "proxy.weight"
	Health      = "proxy.health"
	Strip       = "proxy.strip"
	Name        = "proxy.name"
	RateLimit   = "proxy.ratelimit"
	RateRPM     = "proxy.ratelimit.rpm"
	Sticky      = "proxy.sticky"
	Cache       = "proxy.cache"
	CachePaths  = "proxy.cache.paths"
	DropHeaders = "proxy.drop_headers"
	AutoUpdate  = "proxy.autoupdate"
	Unscalable  = "proxy.unscalable"
	Drain       = "proxy.drain"
	Group       = "proxy.group"
	Maintenance = "proxy.maintenance"
	Overlap     = "proxy.overlap"
	ServiceKey  = "proxy.service"
)

// Mirrors of cmd/dashboard constants (internal/ cannot import cmd/*).
const (
	// MaxWeight mirrors maxServiceWeight in cmd/dashboard/validate.go.
	MaxWeight = 100
	// MaxDrainSeconds mirrors maxDrainSeconds in cmd/dashboard/docker.go.
	MaxDrainSeconds = 300
)

// serviceNameRE mirrors serviceNameRE in cmd/dashboard/validate.go.
var serviceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// Limits on a central label set.
const (
	MaxValueBytes = 1 << 10
	MaxKeys       = 64
	maxNameRunes  = 64
)

// Redis key names. Service names cannot contain ':', so only the reserved
// names below can collide with the global keys.
const (
	RedisPrefix     = "pmgr:labels:"
	RedisIndex      = "pmgr:labels:index"
	RedisVersion    = "pmgr:labels:version"
	RedisChannel    = "pmgr:labels:changed"
	redisMetaSuffix = ":meta"
)

// RedisServiceKey is the HASH of managed keys for an adopted service.
func RedisServiceKey(svc string) string { return RedisPrefix + svc }

// RedisMetaKey is the HASH of version/audit metadata for an adopted service.
func RedisMetaKey(svc string) string { return RedisPrefix + svc + redisMetaSuffix }

var reservedServices = map[string]bool{"index": true, "version": true}

// ValidService reports whether svc may be stored centrally: the dashboard's
// service-name shape, minus names whose hash key would collide with
// RedisIndex/RedisVersion.
func ValidService(svc string) bool {
	return serviceNameRE.MatchString(svc) && !reservedServices[svc]
}

var registry = map[string]Spec{}

func add(key string, class Class, consumer string, v func(string) error) {
	registry[key] = Spec{Key: key, Class: class, Validate: v, Consumer: consumer}
}

func init() {
	add(Weight, ProxyLive, "proxy router: backend weight", intRange(1, MaxWeight))
	add(Health, ProxyLive, "proxy health checks", validAbsPath)
	add(Strip, ProxyLive, "proxy router: strip path prefix", validBool)
	add(Name, ProxyLive, "proxy router: display name", validName)
	add(RateLimit, ProxyLive, "proxy rate limiter", validBool)
	add(RateRPM, ProxyLive, "proxy rate limiter", intRange(1, 1<<31-1))
	add(Sticky, ProxyLive, "proxy router: sticky sessions", validBool)
	add(Cache, ProxyLive, "proxy micro-cache", func(s string) error { _, err := ParseCacheTTL(s); return err })
	add(CachePaths, ProxyLive, "proxy micro-cache", validPathList)
	add(DropHeaders, ProxyLive, "proxy router: request header strip (tighten-only)", validHeaderList)
	add(AutoUpdate, DashboardLive, "dashboard autoupdate engine", validBool)
	add(Unscalable, DashboardLive, "dashboard scaling guard", validBool)
	add(Drain, DashboardLive, "dashboard stop grace", intRange(0, MaxDrainSeconds))
	add(Group, DashboardLive, "dashboard status grouping", validGroup)
	add(Maintenance, DashboardLive, "dashboard maintenance page", validAbsPath)
	add(Overlap, DashboardLive, "dashboard overlap restarts", validBool)

	for _, k := range []string{"proxy.auth", "proxy.auth.users", "proxy.auth.mode"} {
		add(k, Security, "proxy auth gate (container-only)", nil)
	}
	for _, k := range []string{"proxy.enable", ServiceKey, "proxy.host", "proxy.port", "proxy.path"} {
		add(k, Identity, "routing identity (recreate)", nil)
	}
	for _, k := range []string{"proxy.canary", "proxy.previous_image", "proxy.spread"} {
		add(k, PerReplica, "dashboard per-replica state", nil)
	}
}

// perReplicaPrefixes are whole label namespaces owned per replica.
var perReplicaPrefixes = []string{"proxy.ab.", "pmgr.env."}

// Lookup returns the registry spec for key. proxy.ab.* and pmgr.env.* map
// to a synthetic PerReplica spec.
func Lookup(key string) (Spec, bool) {
	if s, ok := registry[key]; ok {
		return s, true
	}
	for _, p := range perReplicaPrefixes {
		if strings.HasPrefix(key, p) {
			return Spec{Key: key, Class: PerReplica, Consumer: "dashboard per-replica state"}, true
		}
	}
	return Spec{}, false
}

// Managed reports whether key can be set centrally.
func Managed(key string) bool {
	s, ok := registry[key]
	return ok && (s.Class == ProxyLive || s.Class == DashboardLive)
}

// ManagedKeys returns every managed key, sorted.
func ManagedKeys() []string {
	var out []string
	for k := range registry {
		if Managed(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateValue checks one managed key/value pair, including the generic
// size and control-character limits.
func ValidateValue(key, value string) error {
	if !Managed(key) {
		return keyError(key)
	}
	if value == "" {
		return fmt.Errorf("%s: empty value (use unset instead)", key)
	}
	if len(value) > MaxValueBytes {
		return fmt.Errorf("%s: value longer than %d bytes", key, MaxValueBytes)
	}
	if hasControl(value) {
		return fmt.Errorf("%s: value contains control characters", key)
	}
	if err := registry[key].Validate(value); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// ValidateChange checks a set/unset request against the current managed map
// of a service. allowLoosen permits removing headers from proxy.drop_headers.
func ValidateChange(current, set map[string]string, unset []string, allowLoosen bool) error {
	next := make(map[string]string, len(current)+len(set))
	for k, v := range current {
		next[k] = v
	}
	for _, k := range unset {
		if _, both := set[k]; both {
			return fmt.Errorf("%s: both set and unset", k)
		}
		if !Managed(k) {
			return keyError(k)
		}
		delete(next, k)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := ValidateValue(k, set[k]); err != nil {
			return err
		}
		next[k] = set[k]
	}
	if len(next) > MaxKeys {
		return fmt.Errorf("too many keys: %d > %d", len(next), MaxKeys)
	}
	if !allowLoosen {
		kept := map[string]bool{}
		for _, h := range splitList(next[DropHeaders]) {
			kept[textproto.CanonicalMIMEHeaderKey(h)] = true
		}
		for _, h := range splitList(current[DropHeaders]) {
			if !kept[textproto.CanonicalMIMEHeaderKey(h)] {
				return fmt.Errorf("%s: removing %q loosens request filtering (set allow_loosen to confirm)", DropHeaders, h)
			}
		}
	}
	return nil
}

func keyError(key string) error {
	s, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("%s: not a central label (allowed: %s)", key, strings.Join(ManagedKeys(), ", "))
	}
	return fmt.Errorf("%s: %s label, not settable centrally (allowed: %s)", key, s.Class, strings.Join(ManagedKeys(), ", "))
}

// ParseCacheTTL parses the proxy.cache label / routes.json "cache" value.
// The off spellings mirror the boolean labels so "false" on a cache label
// reads the way an operator expects.
func ParseCacheTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "0", "false", "off":
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return d, nil
}

func validBool(s string) error {
	if s != "true" && s != "false" {
		return fmt.Errorf("want true or false, got %q", s)
	}
	return nil
}

func intRange(lo, hi int) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("want an integer %d-%d, got %q", lo, hi, s)
		}
		return nil
	}
}

func validAbsPath(s string) error {
	if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \t") {
		return fmt.Errorf("want an absolute path starting with /, got %q", s)
	}
	return nil
}

func validName(s string) error {
	if utf8.RuneCountInString(s) > maxNameRunes {
		return fmt.Errorf("longer than %d characters", maxNameRunes)
	}
	return nil
}

func validGroup(s string) error {
	if !serviceNameRE.MatchString(s) {
		return fmt.Errorf("invalid group name %q", s)
	}
	return nil
}

func validPathList(s string) error {
	ps := splitList(s)
	if len(ps) == 0 {
		return fmt.Errorf("empty path list")
	}
	for _, p := range ps {
		if err := validAbsPath(p); err != nil {
			return err
		}
	}
	return nil
}

func validHeaderList(s string) error {
	hs := splitList(s)
	if len(hs) == 0 {
		return fmt.Errorf("empty header list")
	}
	for _, h := range hs {
		for _, r := range h {
			if !isTokenRune(r) {
				return fmt.Errorf("invalid header name %q", h)
			}
		}
	}
	return nil
}

// isTokenRune is the RFC 7230 tchar set.
func isTokenRune(r rune) bool {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// splitList mirrors cmd/proxy's splitTrimmed: comma-split, trim, drop empties.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
