package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The list_* MCP tools answer with a one-line-per-item text summary unless
// the caller names one item, which gets full JSON. Only the MCP-facing
// rendering lives here — the /api/* responses these read are untouched. A
// body that isn't the expected array passes through pretty-printed rather
// than hiding the real response behind a decode error.

// dnsContentMax caps a record's content in the list_dns summary, in runes.
const dnsContentMax = 80

// sortedUnique returns the non-empty values of in, sorted and de-duplicated.
func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// renderServices renders a GET /api/services body: the summary when service
// is empty, else full JSON (minus the raw Labels map) for every entry with
// that name — one per machine it runs on.
func renderServices(b []byte, service string) (string, error) {
	var svcs []Service
	if err := json.Unmarshal(b, &svcs); err != nil {
		return pretty(b), nil
	}
	sort.SliceStable(svcs, func(i, j int) bool {
		if svcs[i].Name != svcs[j].Name {
			return svcs[i].Name < svcs[j].Name
		}
		return svcs[i].Machine < svcs[j].Machine
	})

	if service != "" {
		match := []Service{}
		names := make([]string, 0, len(svcs))
		for _, s := range svcs {
			names = append(names, s.Name)
			if s.Name == service {
				s.Labels = nil
				match = append(match, s)
			}
		}
		if len(match) == 0 {
			if names = sortedUnique(names); len(names) == 0 {
				return "", fmt.Errorf("no service named %q — no services are managed", service)
			}
			return "", fmt.Errorf("no service named %q — valid names: %s", service, strings.Join(names, ", "))
		}
		out, err := json.Marshal(match)
		if err != nil {
			return pretty(b), nil
		}
		return pretty(out), nil
	}

	if len(svcs) == 0 {
		return "No services.", nil
	}
	lines := []string{fmt.Sprintf("%d services — summary. Pass service=\"<name>\" for full JSON detail.", len(svcs))}
	for _, s := range svcs {
		lines = append(lines, serviceSummaryLine(s))
	}
	return strings.Join(lines, "\n"), nil
}

func serviceSummaryLine(s Service) string {
	parts := []string{s.Name}
	if s.Machine != "" {
		parts = append(parts, "machine="+s.Machine)
	}
	if s.Host == "" {
		parts = append(parts, "unrouted")
	} else {
		parts = append(parts, s.Host+s.Path)
	}
	parts = append(parts, "image="+s.Image)

	var members []string
	if len(s.MemberSummaries) > 0 {
		running, total := 0, 0
		for _, m := range s.MemberSummaries {
			// Canaries are listed under members= but kept out of the count,
			// which is the serving replica set restart_replica can act on.
			if !m.IsCanary {
				total++
				if m.State == "running" {
					running++
				}
			}
			var marks []string
			if m.IsCanary {
				marks = append(marks, "canary")
			}
			if m.State != "running" {
				marks = append(marks, m.State)
			}
			if m.Health == "unhealthy" || m.Health == "starting" {
				marks = append(marks, m.Health)
			}
			name := m.Name
			if len(marks) > 0 {
				name += "[" + strings.Join(marks, ",") + "]"
			}
			members = append(members, name)
		}
		parts = append(parts, fmt.Sprintf("replicas=%d/%d", running, total))
	} else {
		parts = append(parts, fmt.Sprintf("replicas=%d", s.Replicas))
	}

	var flags []string
	if s.AllStopped {
		flags = append(flags, "all-stopped")
	}
	if s.UpdateAvailable {
		flags = append(flags, "update-available")
	}
	if s.AutoUpdateSkipReason != "" {
		flags = append(flags, "update-blocked")
	}
	if s.ImageCheckError != "" {
		flags = append(flags, "image-check-error")
	}
	// An A/B test's B variant is its canary set, so the ab-test flag
	// stands in for canary there.
	if s.CanaryImage != "" && s.ABTest == nil {
		flags = append(flags, fmt.Sprintf("canary:%sx%d", s.CanaryImage, s.CanaryReplicas))
	}
	if s.ABTest != nil {
		flags = append(flags, "ab-test:"+s.ABTest.Phase)
		if s.ABTest.Pending != "" {
			flags = append(flags, "ab-pending:"+s.ABTest.Pending)
		}
	}
	if s.AutoUpdate {
		flags = append(flags, "auto-update")
	}
	if s.Unscalable {
		flags = append(flags, "unscalable")
	}
	if s.Overlap {
		flags = append(flags, "overlap")
	}
	if s.Onboarded {
		flags = append(flags, "onboarded")
	}
	if s.DualTracked {
		flags = append(flags, "dual-tracked")
	}
	// /api/services has no central-env field; the origin label on the
	// service's containers is the only trace of it there.
	if s.Labels[labelEnvOrigin] != "" {
		flags = append(flags, "central-env")
	}
	if s.Weight > 1 {
		flags = append(flags, fmt.Sprintf("weight:%d", s.Weight))
	}
	if len(flags) > 0 {
		parts = append(parts, "flags="+strings.Join(flags, ","))
	}
	if len(members) > 0 {
		parts = append(parts, "members="+strings.Join(members, ","))
	}
	return strings.Join(parts, "  ")
}

// renderRoutes renders a GET /api/routes body: the summary when hostname is
// empty, else the untouched JSON of every route on that hostname.
func renderRoutes(b []byte, hostname string) (string, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return pretty(b), nil
	}
	routes := make([]RouteView, len(raws))
	for i, raw := range raws {
		if err := json.Unmarshal(raw, &routes[i]); err != nil {
			return pretty(b), nil
		}
	}

	if hostname != "" {
		match := []json.RawMessage{}
		hosts := make([]string, 0, len(routes))
		for i, r := range routes {
			hosts = append(hosts, r.Host)
			if strings.EqualFold(r.Host, hostname) {
				match = append(match, raws[i])
			}
		}
		if len(match) == 0 {
			return "", fmt.Errorf("no route for hostname %q — routed hosts: %s", hostname, strings.Join(sortedUnique(hosts), ", "))
		}
		out, err := json.Marshal(match)
		if err != nil {
			return pretty(b), nil
		}
		return pretty(out), nil
	}

	if len(routes) == 0 {
		return "No routes.", nil
	}
	lines := []string{fmt.Sprintf("%d routes — summary. Pass hostname=\"<hostname>\" for full JSON detail.", len(routes))}
	for _, r := range routes {
		lines = append(lines, routeSummaryLine(r))
	}
	return strings.Join(lines, "\n"), nil
}

func routeSummaryLine(r RouteView) string {
	const maxBackends = 4
	parts := []string{r.Host + r.Path}
	if r.Service != "" {
		parts = append(parts, "service="+r.Service)
	}
	if r.Name != "" {
		parts = append(parts, "name="+r.Name)
	}

	var flags []string
	if r.Strip {
		flags = append(flags, "strip")
	}
	if len(r.Backends) == 0 {
		parts = append(parts, "backends=0")
		flags = append(flags, "no-backends")
	} else {
		static := 0
		var shown []string
		for i, be := range r.Backends {
			isStatic := be.Container == "static" || be.Container == ""
			if isStatic {
				static++
			}
			if i >= maxBackends {
				continue
			}
			label := be.Container
			if isStatic {
				label = be.URL
			}
			if be.Weight != 1 {
				label += fmt.Sprintf("(w=%d)", be.Weight)
			}
			shown = append(shown, label)
		}
		if extra := len(r.Backends) - maxBackends; extra > 0 {
			shown = append(shown, fmt.Sprintf("+%d more", extra))
		}
		src := "mixed"
		switch static {
		case len(r.Backends):
			src = "static"
		case 0:
			src = "containers"
		}
		parts = append(parts, "source="+src, fmt.Sprintf("backends=%d: %s", len(r.Backends), strings.Join(shown, ", ")))
	}
	if len(flags) > 0 {
		parts = append(parts, "flags="+strings.Join(flags, ","))
	}
	return strings.Join(parts, "  ")
}

// renderDNS renders a GET /api/cf/records body: the summary when name is
// empty, else the untouched JSON of the records named name
// (case-insensitive), or failing an exact match those containing it.
func renderDNS(b []byte, name string) (string, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return pretty(b), nil
	}
	recs := make([]DNSRecord, len(raws))
	for i, raw := range raws {
		if err := json.Unmarshal(raw, &recs[i]); err != nil {
			return pretty(b), nil
		}
	}

	if name != "" {
		want := strings.ToLower(name)
		exact, partial := []json.RawMessage{}, []json.RawMessage{}
		for i, r := range recs {
			got := strings.ToLower(r.Name)
			if got == want {
				exact = append(exact, raws[i])
			} else if strings.Contains(got, want) {
				partial = append(partial, raws[i])
			}
		}
		match := exact
		if len(match) == 0 {
			match = partial
		}
		if len(match) == 0 {
			return "", fmt.Errorf("no DNS record matching %q among %d records in this zone — call list_dns without name for the summary", name, len(recs))
		}
		out, err := json.Marshal(match)
		if err != nil {
			return pretty(b), nil
		}
		return pretty(out), nil
	}

	if len(recs) == 0 {
		return "No DNS records.", nil
	}
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Name != recs[j].Name {
			return recs[i].Name < recs[j].Name
		}
		return recs[i].Type < recs[j].Type
	})
	lines := []string{fmt.Sprintf("%d DNS records — summary. Pass name=\"<record name>\" for full JSON detail. The id is what update_dns_record/delete_dns_record take.", len(recs))}
	for _, r := range recs {
		content := r.Content
		if rs := []rune(content); len(rs) > dnsContentMax {
			content = string(rs[:dnsContentMax]) + "…"
		}
		proxied := "dns-only"
		if r.Proxied {
			proxied = "proxied"
		}
		parts := []string{r.Type, r.Name, "->", content, proxied}
		// Cloudflare reports an automatic TTL as 1.
		if r.TTL != 1 {
			parts = append(parts, fmt.Sprintf("ttl=%d", r.TTL))
		}
		parts = append(parts, "id="+r.ID)
		lines = append(lines, strings.Join(parts, "  "))
	}
	return strings.Join(lines, "\n"), nil
}

// renderLogs renders a GET /api/logs/{container} body as plain text: a
// "[stream]" header only where the stream changes, then that run's lines,
// each prefixed with its UTC time when known, with consecutive identical
// lines collapsed to one "first..last text  (×N)". Accepts
// the bare line array or an object carrying it under "lines".
func renderLogs(b []byte) string {
	var lines []logLine
	if err := json.Unmarshal(b, &lines); err != nil {
		var wrapped struct {
			Lines []logLine `json:"lines"`
		}
		if err := json.Unmarshal(b, &wrapped); err != nil || wrapped.Lines == nil {
			return pretty(b)
		}
		lines = wrapped.Lines
	}
	if len(lines) == 0 {
		return "0 log lines."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d log lines — grouped by stream, times UTC, consecutive repeats collapsed as (×N).\n", len(lines))
	stream := ""
	for i := 0; i < len(lines); {
		l := lines[i]
		n := 1
		for i+n < len(lines) && lines[i+n].Stream == l.Stream && lines[i+n].Text == l.Text {
			n++
		}
		if l.Stream != stream {
			stream = l.Stream
			fmt.Fprintf(&sb, "[%s]\n", stream)
		}
		if l.TS != "" {
			sb.WriteString(l.TS)
			if last := lines[i+n-1].TS; n > 1 && last != "" && last != l.TS {
				sb.WriteString(".." + last)
			}
			sb.WriteByte(' ')
		}
		sb.WriteString(l.Text)
		if n > 1 {
			fmt.Fprintf(&sb, "  (×%d)", n)
		}
		sb.WriteByte('\n')
		i += n
	}
	return strings.TrimRight(sb.String(), "\n")
}
