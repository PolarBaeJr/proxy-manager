package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const cfBaseURL = "https://api.cloudflare.com/client/v4"

type cloudflareClient struct {
	token  string
	zoneID string
	domain string
	http   *http.Client
}

func newCloudflareClient(token, zoneID, domain string) *cloudflareClient {
	return &cloudflareClient{
		token:  token,
		zoneID: zoneID,
		domain: domain,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
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
	req, _ := http.NewRequestWithContext(ctx, method, cfBaseURL+path, rdr)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("cloudflare %s %s: %d %s", method, path, resp.StatusCode, string(b))
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

func (c *cloudflareClient) findByName(ctx context.Context, name string) (*DNSRecord, error) {
	body, err := c.do(ctx, "GET", "/zones/"+c.zoneID+"/dns_records?name="+c.fqdn(name), nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result []DNSRecord `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Result) == 0 {
		return nil, fmt.Errorf("no record %q", name)
	}
	return &resp.Result[0], nil
}

type CreateDNSRequest struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func (c *cloudflareClient) Create(ctx context.Context, req CreateDNSRequest) (*DNSRecord, error) {
	if req.Type == "" {
		req.Type = "CNAME"
	}
	if req.TTL == 0 {
		req.TTL = 1 // automatic
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
