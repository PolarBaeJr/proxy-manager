package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const cfBaseURL = "https://api.cloudflare.com/client/v4"

// cfZoneSyncInterval paces zone discovery. Zones are added to an account by
// hand, minutes apart at most, so this is slow on purpose.
const cfZoneSyncInterval = 5 * time.Minute

type cloudflareClient struct {
	token   string
	zoneID  string
	domain  string
	baseURL string
	http    *http.Client
}

func newCloudflareClient(token, zoneID, domain string) *cloudflareClient {
	return &cloudflareClient{
		token:   token,
		zoneID:  zoneID,
		domain:  domain,
		baseURL: cfBaseURL,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// cfAPIError is a non-2xx response from the Cloudflare API. It carries the
// status so callers can distinguish "this token can't see this zone" (401/403)
// from a transient failure.
type cfAPIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e cfAPIError) Error() string {
	return fmt.Sprintf("cloudflare %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// cfStatus returns the HTTP status behind err, or 0 if err isn't a cfAPIError.
func cfStatus(err error) int {
	var e cfAPIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// DNSRecord is a simplified view of a Cloudflare DNS record for the UI.
type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func (c *cloudflareClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, cfAPIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(b)}
	}
	return b, nil
}

func (c *cloudflareClient) List(ctx context.Context) ([]DNSRecord, error) {
	body, err := c.do(ctx, "GET", "/zones/"+c.zoneID+"/dns_records?per_page=200", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result []DNSRecord `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	sort.Slice(resp.Result, func(i, j int) bool {
		if resp.Result[i].Name != resp.Result[j].Name {
			return resp.Result[i].Name < resp.Result[j].Name
		}
		return resp.Result[i].Type < resp.Result[j].Type
	})
	return resp.Result, nil
}

func (c *cloudflareClient) fqdn(name string) string {
	if c.domain == "" || strings.Contains(name, ".") {
		return name
	}
	return name + "." + c.domain
}

// validateName rejects a fully-qualified name that belongs to a different zone
// than this client's — with multiple zones registered, sending it anyway would
// just produce a confusing Cloudflare-side error.
func (c *cloudflareClient) validateName(name string) error {
	if c.domain == "" || !strings.Contains(name, ".") {
		return nil
	}
	n, d := strings.ToLower(name), strings.ToLower(c.domain)
	if n == d || strings.HasSuffix(n, "."+d) {
		return nil
	}
	return fmt.Errorf("name %q is not in zone %s", name, c.domain)
}

type CreateDNSRequest struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"` // MX records only
}

func (c *cloudflareClient) Create(ctx context.Context, req CreateDNSRequest) (*DNSRecord, error) {
	if req.Type == "" {
		req.Type = "CNAME"
	}
	if req.TTL == 0 {
		req.TTL = 1 // automatic
	}
	if err := c.validateName(req.Name); err != nil {
		return nil, err
	}
	req.Name = c.fqdn(req.Name)
	body, err := c.do(ctx, "POST", "/zones/"+c.zoneID+"/dns_records", req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result DNSRecord `json:"result"`
	}
	return &resp.Result, json.Unmarshal(body, &resp)
}

type UpdateDNSRequest struct {
	Content *string `json:"content,omitempty"`
	Proxied *bool   `json:"proxied,omitempty"`
	Name    *string `json:"name,omitempty"`
}

func (c *cloudflareClient) Update(ctx context.Context, id string, req UpdateDNSRequest) (*DNSRecord, error) {
	body, err := c.do(ctx, "PATCH", "/zones/"+c.zoneID+"/dns_records/"+id, req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result DNSRecord `json:"result"`
	}
	return &resp.Result, json.Unmarshal(body, &resp)
}

func (c *cloudflareClient) Delete(ctx context.Context, id string) error {
	_, err := c.do(ctx, "DELETE", "/zones/"+c.zoneID+"/dns_records/"+id, nil)
	return err
}

// ---- Multi-zone registry ----

// cfZoneState tracks one zone's client plus whether the last API call was
// accepted — a token scoped to only some zones fails lazily, on first use.
type cfZoneState struct {
	client  *cloudflareClient
	ok      bool
	lastErr string
	// fromEnv marks a zone pinned by CLOUDFLARE_ZONE_ID / CLOUDFLARE_ZONES.
	// Discovery may never overwrite or remove one: an env entry can carry an
	// explicit per-zone token that a listing can't infer.
	fromEnv bool
}

// parseCloudflareZones parses CLOUDFLARE_ZONES: comma-separated entries of
// "domain:zoneid" or "domain:zoneid:token". Malformed entries are skipped (with
// a human-readable reason) rather than failing the whole dashboard.
func parseCloudflareZones(spec, defaultToken string) ([]*cloudflareClient, []string) {
	var clients []*cloudflareClient
	var skipped []string
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 2 && len(parts) != 3 {
			skipped = append(skipped, fmt.Sprintf("%q: want domain:zoneid[:token]", entry))
			continue
		}
		domain := strings.ToLower(strings.TrimSpace(parts[0]))
		zoneID := strings.TrimSpace(parts[1])
		if domain == "" || zoneID == "" {
			skipped = append(skipped, fmt.Sprintf("%q: empty domain or zone id", entry))
			continue
		}
		token := defaultToken
		if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
			token = strings.TrimSpace(parts[2])
		}
		clients = append(clients, newCloudflareClient(token, zoneID, domain))
	}
	return clients, skipped
}

// cfZone is one entry from GET /zones.
type cfZone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ListZones asks Cloudflare which zones this token can see. It's what makes
// adding a domain a zero-config operation: put the domain in the Cloudflare
// account the token is scoped for and the dashboard picks it up on its own.
//
// Uses the client's token but none of its zone state, so any client with a
// usable token can serve as the discovery client.
func (c *cloudflareClient) ListZones(ctx context.Context) ([]cfZone, error) {
	body, err := c.do(ctx, "GET", "/zones?per_page=50", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result []cfZone `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// cloudflareRegistry maps a domain to the client that can edit its zone. The
// domain is only ever a map key — it never reaches a Cloudflare URL.
type cloudflareRegistry struct {
	mu       sync.RWMutex
	order    []string
	byDomain map[string]*cfZoneState
	def      string

	// discoverToken is the token Sync registers newly found zones with — the
	// account-wide CLOUDFLARE_API_TOKEN. Empty disables discovery entirely.
	discoverToken string
	// discover lists the account's zones. Held as a field rather than built
	// per call so a test can point it at a stub server.
	discover *cloudflareClient
}

// newCloudflareRegistryFromEnv builds the registry from the legacy single-zone
// vars (which still define the DEFAULT zone) plus CLOUDFLARE_ZONES. Returns nil
// when no zone is configured, along with lines the caller should log.
func newCloudflareRegistryFromEnv(getenv func(string) string) (*cloudflareRegistry, []string) {
	var msgs []string
	r := &cloudflareRegistry{byDomain: map[string]*cfZoneState{}}

	defToken := strings.TrimSpace(getenv("CLOUDFLARE_API_TOKEN"))
	if defToken != "" {
		r.discoverToken = defToken
		r.discover = newCloudflareClient(defToken, "", "")
	}
	if zoneID := getenv("CLOUDFLARE_ZONE_ID"); zoneID != "" && defToken != "" {
		domain := strings.ToLower(strings.TrimSpace(getenv("CLOUDFLARE_DOMAIN")))
		key := domain
		if key == "" {
			key = zoneID
		}
		r.add(key, newCloudflareClient(defToken, zoneID, domain))
		r.def = key
	}

	clients, skipped := parseCloudflareZones(getenv("CLOUDFLARE_ZONES"), defToken)
	for _, s := range skipped {
		msgs = append(msgs, "⚠ CLOUDFLARE_ZONES skipped "+s)
	}
	for _, c := range clients {
		if c.token == "" {
			msgs = append(msgs, "⚠ CLOUDFLARE_ZONES skipped "+c.domain+": no API token (set CLOUDFLARE_API_TOKEN or a per-zone token)")
			continue
		}
		if _, dup := r.byDomain[c.domain]; dup {
			msgs = append(msgs, "⚠ CLOUDFLARE_ZONES skipped "+c.domain+": already registered")
			continue
		}
		r.add(c.domain, c)
	}

	// A bare token with no zone vars is now a valid config: Sync will populate
	// the registry from the account. Without one there is nothing to discover
	// with, so an empty registry stays nil and the integration reports off.
	if len(r.order) == 0 && defToken == "" {
		return nil, msgs
	}
	if r.def == "" && len(r.order) > 0 {
		r.def = r.order[0]
	}
	return r, msgs
}

// keyForZoneID finds an existing entry pointing at the same Cloudflare zone,
// whatever key it happens to be registered under. Caller holds the lock.
func (r *cloudflareRegistry) keyForZoneID(id string) (string, bool) {
	for key, st := range r.byDomain {
		if st.client.zoneID == id {
			return key, true
		}
	}
	return "", false
}

func (r *cloudflareRegistry) add(key string, c *cloudflareClient) {
	r.order = append(r.order, key)
	r.byDomain[key] = &cfZoneState{client: c, ok: true, fromEnv: true}
}

// Sync reconciles the registry against the zones the token can currently see,
// so adding a domain to the Cloudflare account is the only step needed to get
// it into the DNS tab — no env edit, no redeploy.
//
// Reconciling (not just adding) is the point: a zone removed from the account
// has to leave the registry too, or the UI keeps a dead chip that the browser's
// saved zone preference can pin itself to.
//
// Env-pinned zones are never touched, in either direction.
func (r *cloudflareRegistry) Sync(ctx context.Context) error {
	if r == nil || r.discover == nil {
		return nil
	}
	zones, err := r.discover.ListZones(ctx)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	seen := map[string]bool{}
	for _, z := range zones {
		name := strings.ToLower(strings.TrimSpace(z.Name))
		// A pending / moved / deactivated zone would accept API calls but not
		// actually serve DNS, so surfacing it as editable would be misleading.
		if name == "" || z.ID == "" || z.Status != "active" {
			continue
		}
		seen[name] = true
		if st, ok := r.byDomain[name]; ok {
			// Refresh the zone id so a domain moved between accounts keeps
			// working, but leave env entries and their tokens alone.
			//
			// Swap the whole client rather than assigning through the pointer:
			// Lookup hands the *cloudflareClient to a request handler and then
			// releases the read lock, so mutating a field a handler may be
			// reading is a race. Replacing the pointer is covered by the write
			// lock held here, and an in-flight handler simply finishes against
			// the old client.
			if !st.fromEnv && st.client.zoneID != z.ID {
				st.client = newCloudflareClient(r.discoverToken, z.ID, name)
			}
			continue
		}
		// A legacy CLOUDFLARE_ZONE_ID with no CLOUDFLARE_DOMAIN is keyed by its
		// zone id, so the same zone would otherwise be added a second time
		// under its name — two chips in the DNS tab for one zone.
		if key, dup := r.keyForZoneID(z.ID); dup {
			seen[key] = true
			continue
		}
		r.order = append(r.order, name)
		r.byDomain[name] = &cfZoneState{
			client: newCloudflareClient(r.discoverToken, z.ID, name),
			ok:     true,
		}
	}

	kept := r.order[:0]
	for _, key := range r.order {
		st := r.byDomain[key]
		if st.fromEnv || seen[key] {
			kept = append(kept, key)
			continue
		}
		delete(r.byDomain, key)
	}
	r.order = kept

	// The default zone must always name a live entry: DefaultDomain and every
	// Lookup("") call route through it.
	if _, ok := r.byDomain[r.def]; !ok {
		r.def = ""
		if len(r.order) > 0 {
			r.def = r.order[0]
		}
	}
	return nil
}

// SyncLoop discovers zones at startup and re-checks on a slow timer — a domain
// added to Cloudflare shows up without a restart, and one removed disappears.
func (r *cloudflareRegistry) SyncLoop(ctx context.Context) {
	if r == nil || r.discover == nil {
		return
	}
	t := time.NewTicker(cfZoneSyncInterval)
	defer t.Stop()
	for {
		if err := r.Sync(ctx); err != nil {
			// Most likely the token lacks the account-level Zone:Read scope.
			// Not fatal: env-configured zones keep working exactly as before.
			log.Printf("⚠ cloudflare zone discovery: %v", err)
		} else {
			log.Printf("cloudflare zones: %s", strings.Join(r.Domains(), ", "))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Lookup resolves a domain to its client. An empty domain means the default
// zone, which is what keeps single-zone callers working unchanged.
func (r *cloudflareRegistry) Lookup(domain string) (*cloudflareClient, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := strings.ToLower(strings.TrimSpace(domain))
	if key == "" {
		key = r.def
	}
	st, ok := r.byDomain[key]
	if !ok {
		return nil, false
	}
	return st.client, true
}

func (r *cloudflareRegistry) Default() *cloudflareClient {
	c, _ := r.Lookup("")
	return c
}

func (r *cloudflareRegistry) DefaultDomain() string {
	c := r.Default()
	if c == nil {
		return ""
	}
	return c.domain
}

// Usable reports whether any zone is actually registered. A token with no
// pinned zones yields a live-but-empty registry until discovery lands (or
// permanently, if the token lacks account-level Zone:Read), and the DNS tab
// must read that as "off" rather than showing itself with nothing in it.
func (r *cloudflareRegistry) Usable() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order) > 0
}

// Domains lists the registered zone keys in registration order (default first).
func (r *cloudflareRegistry) Domains() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// noteResult records the outcome of a real API call so the UI can flag a zone
// the token isn't scoped for. Only auth failures are sticky — a transient
// network error shouldn't mark a zone unusable.
func (r *cloudflareRegistry) noteResult(domain string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(domain))
	if key == "" {
		key = r.def
	}
	st, ok := r.byDomain[key]
	if !ok {
		return
	}
	switch code := cfStatus(err); {
	case err == nil:
		st.ok, st.lastErr = true, ""
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		st.ok = false
		// Deliberately generic: cfAPIError.Error() embeds "/zones/<zoneid>/…",
		// and this string is served to the browser by /api/cf/enabled.
		st.lastErr = fmt.Sprintf("token lacks permission for this zone (%d)", code)
	}
}

// Status is what /api/cf/enabled reports. It deliberately carries no zone IDs
// or tokens — only the domain, usability, and the last auth error. It reports
// client.domain and not the map key because a legacy zone with no
// CLOUDFLARE_DOMAIN is keyed by its zone id, which must not reach the browser.
func (r *cloudflareRegistry) Status() []map[string]any {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.order))
	for _, key := range r.order {
		st := r.byDomain[key]
		source := "cloudflare"
		if st.fromEnv {
			source = "env"
		}
		out = append(out, map[string]any{
			"domain": st.client.domain,
			"ok":     st.ok,
			"error":  st.lastErr,
			// Where the zone came from — safe to expose (unlike the zone id),
			// and it explains to the operator why a chip appeared on its own.
			"source": source,
		})
	}
	return out
}
