package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const cfBaseURL = "https://api.cloudflare.com/client/v4"

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

// cloudflareRegistry maps a domain to the client that can edit its zone. The
// domain is only ever a map key — it never reaches a Cloudflare URL.
type cloudflareRegistry struct {
	mu       sync.RWMutex
	order    []string
	byDomain map[string]*cfZoneState
	def      string
}

// newCloudflareRegistryFromEnv builds the registry from the legacy single-zone
// vars (which still define the DEFAULT zone) plus CLOUDFLARE_ZONES. Returns nil
// when no zone is configured, along with lines the caller should log.
func newCloudflareRegistryFromEnv(getenv func(string) string) (*cloudflareRegistry, []string) {
	var msgs []string
	r := &cloudflareRegistry{byDomain: map[string]*cfZoneState{}}

	defToken := getenv("CLOUDFLARE_API_TOKEN")
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

	if len(r.order) == 0 {
		return nil, msgs
	}
	if r.def == "" {
		r.def = r.order[0]
	}
	return r, msgs
}

func (r *cloudflareRegistry) add(key string, c *cloudflareClient) {
	r.order = append(r.order, key)
	r.byDomain[key] = &cfZoneState{client: c, ok: true}
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
		out = append(out, map[string]any{
			"domain": st.client.domain,
			"ok":     st.ok,
			"error":  st.lastErr,
		})
	}
	return out
}
