package labels

import (
	"strings"
	"testing"
	"time"
)

func TestValidateValueTable(t *testing.T) {
	cases := []struct {
		key, val string
		ok       bool
	}{
		{Weight, "1", true},
		{Weight, "100", true},
		{Weight, "0", false},
		{Weight, "101", false},
		{Weight, "x", false},
		{Health, "/healthz", true},
		{Health, "healthz", false},
		{Strip, "true", true},
		{Strip, "false", true},
		{Strip, "yes", false},
		{Name, "My App", true},
		{Name, strings.Repeat("a", 65), false},
		{RateLimit, "true", true},
		{RateRPM, "60", true},
		{RateRPM, "0", false},
		{RateRPM, "-5", false},
		{Sticky, "false", true},
		{Cache, "30s", true},
		{Cache, "off", true},
		{Cache, "-1s", false},
		{Cache, "soon", false},
		{CachePaths, "/a, /b", true},
		{CachePaths, "/a,b", false},
		{CachePaths, " , ", false},
		{DropHeaders, "X-Forwarded-User, Authorization", true},
		{DropHeaders, "Bad Header", false},
		{AutoUpdate, "true", true},
		{Unscalable, "TRUE", false},
		{Drain, "0", true},
		{Drain, "300", true},
		{Drain, "301", false},
		{Group, "my-group", true},
		{Group, "bad/group", false},
		{Maintenance, "/srv/maint.html", true},
		{Maintenance, "maint.html", false},
		{Overlap, "true", true},
		{Overlap, "false", true},
		{Overlap, "1", false},
		{Weight, "", false},
		{Name, strings.Repeat("a", 2000), false},
		{Name, "a\nb", false},
		{"proxy.host", "x.example.com", false},
		{"proxy.auth", "false", false},
		{"proxy.ab.split", "50", false},
		{"pmgr.env.origin", "pi", false},
		{"proxy.bogus", "1", false},
	}
	for _, c := range cases {
		err := ValidateValue(c.key, c.val)
		if (err == nil) != c.ok {
			t.Errorf("ValidateValue(%q, %q) err=%v, want ok=%v", c.key, c.val, err, c.ok)
		}
	}
}

func TestManagedClasses(t *testing.T) {
	for _, k := range []string{Weight, Health, Strip, Name, RateLimit, RateRPM, Sticky, Cache, CachePaths, DropHeaders, AutoUpdate, Unscalable, Drain, Group, Maintenance, Overlap} {
		if !Managed(k) {
			t.Errorf("%s should be managed", k)
		}
	}
	for _, k := range []string{"proxy.enable", "proxy.service", "proxy.host", "proxy.port", "proxy.path", "proxy.auth", "proxy.auth.users", "proxy.auth.mode", "proxy.canary", "proxy.previous_image", "proxy.spread", "proxy.ab.variant", "pmgr.env.version", "proxy.unknown"} {
		if Managed(k) {
			t.Errorf("%s must not be managed", k)
		}
	}
	if s, ok := Lookup("proxy.ab.id"); !ok || s.Class != PerReplica {
		t.Errorf("proxy.ab.* lookup = %+v, %v", s, ok)
	}
	if s, _ := Lookup(Overlap); s.Class != DashboardLive {
		t.Errorf("proxy.overlap class = %v", s.Class)
	}
}

func TestValidateChangeRejections(t *testing.T) {
	cases := []struct {
		name  string
		set   map[string]string
		unset []string
		want  string
	}{
		{"identity", map[string]string{"proxy.host": "a.example"}, nil, "identity"},
		{"security", map[string]string{"proxy.auth": "false"}, nil, "security"},
		{"per-replica", map[string]string{"proxy.ab.split": "10"}, nil, "per-replica"},
		{"unknown lists allowed", map[string]string{"proxy.nope": "1"}, nil, "allowed: "},
		{"unset identity", nil, []string{"proxy.service"}, "identity"},
		{"set and unset", map[string]string{Weight: "2"}, []string{Weight}, "both"},
		{"bad value", map[string]string{Weight: "999"}, nil, "proxy.weight"},
	}
	for _, c := range cases {
		err := ValidateChange(nil, c.set, c.unset, false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want containing %q", c.name, err, c.want)
		}
	}
	if err := ValidateChange(nil, map[string]string{Weight: "5", Overlap: "true"}, []string{Sticky}, false); err != nil {
		t.Errorf("valid change rejected: %v", err)
	}
}

func TestValidateChangeMaxKeys(t *testing.T) {
	cur := map[string]string{}
	for i := 0; i < MaxKeys; i++ {
		cur["k"+string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}
	if err := ValidateChange(cur, map[string]string{Weight: "2"}, nil, false); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("err=%v, want too many keys", err)
	}
}

func TestDropHeadersTightenOnly(t *testing.T) {
	cur := map[string]string{DropHeaders: "X-User, authorization"}
	if err := ValidateChange(cur, map[string]string{DropHeaders: "Authorization,x-user,X-Extra"}, nil, false); err != nil {
		t.Errorf("tighten (case-insensitive superset) rejected: %v", err)
	}
	if err := ValidateChange(cur, map[string]string{DropHeaders: "X-User"}, nil, false); err == nil {
		t.Error("removing a header accepted without allow_loosen")
	}
	if err := ValidateChange(cur, nil, []string{DropHeaders}, false); err == nil {
		t.Error("unsetting drop_headers accepted without allow_loosen")
	}
	if err := ValidateChange(cur, nil, []string{DropHeaders}, true); err != nil {
		t.Errorf("allow_loosen unset rejected: %v", err)
	}
	if err := ValidateChange(nil, map[string]string{DropHeaders: "X-A"}, nil, false); err != nil {
		t.Errorf("first set rejected: %v", err)
	}
}

func TestValidService(t *testing.T) {
	for svc, want := range map[string]bool{"web": true, "a.b-c_d": true, "index": false, "version": false, "": false, "a:meta": false, "-x": false} {
		if got := ValidService(svc); got != want {
			t.Errorf("ValidService(%q)=%v want %v", svc, got, want)
		}
	}
	if RedisServiceKey("web") != "pmgr:labels:web" || RedisMetaKey("web") != "pmgr:labels:web:meta" {
		t.Error("redis key names changed")
	}
}

func TestParseCacheTTL(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", 0, false}, {"0", 0, false}, {"false", 0, false}, {"OFF", 0, false},
		{" 30s ", 30 * time.Second, false}, {"1m", time.Minute, false},
		{"-5s", 0, true}, {"abc", 0, true},
	}
	for _, c := range cases {
		d, err := ParseCacheTTL(c.in)
		if (err != nil) != c.err || d != c.want {
			t.Errorf("ParseCacheTTL(%q)=%v,%v want %v err=%v", c.in, d, err, c.want, c.err)
		}
	}
}
