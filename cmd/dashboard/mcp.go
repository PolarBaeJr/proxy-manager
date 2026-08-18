// Minimal MCP server over Streamable HTTP, served by the dashboard itself.
//
// Lives in the dashboard binary rather than a separate service so the tools can
// go through the dashboard's own API handlers. A separate container would have
// needed a long-lived API token on disk to call back in — a credential that
// bypasses 2FA and grants everything the dashboard can do. Removing that is the
// point: there is no token for anyone to manage or leak.
//
// Only the subset the spec requires of a tools-only server is implemented:
// initialize, tools/list, tools/call, ping, and the initialized notification.
// No resources, no prompts, no sampling, no server-initiated messages — so the
// SSE half of the transport is deliberately absent and GET returns 405, which
// the spec explicitly allows.
//
// Auth is NOT handled here. The proxy fronts this with proxy.auth.mode=oauth
// and rejects anything without a token whose audience names this exact
// host+path, so by the time a request arrives it is already authorized. Binding
// to a path is what keeps one MCP's token from opening another's.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// protocolVersion is the spec revision this server implements. A client
	// asking for something else still gets this back — per spec the client
	// then decides whether it can proceed.
	protocolVersion = "2025-06-18"

	// JSON-RPC error codes. -32000..-32099 is the implementation-defined
	// range; the rest are from the JSON-RPC 2.0 spec.
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Tool is one callable exposed to the model.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	// Mutating marks a tool that changes the homelab. These are only
	// registered when writes are enabled, so a read-only deployment cannot
	// call one even by guessing its name.
	Mutating bool `json:"-"`

	// ctx carries the caller's attribution assertion; handlers pass it to the
	// dashboard client so an action is audited against the person who asked,
	// not against the shared service token.
	Handler func(ctx context.Context, args map[string]any) (string, error) `json:"-"`
}

// Server is the tool registry plus the HTTP entry point.
type Server struct {
	name    string
	version string
	tools   map[string]Tool
}

func NewServer(name, version string) *Server {
	return &Server{name: name, version: version, tools: map[string]Tool{}}
}

func (s *Server) Register(t Tool) { s.tools[t.Name] = t }

// toolList is sorted so tools/list is stable between calls — an unstable
// ordering shows up as spurious diffs in client-side tool caches.
func (s *Server) toolList() []Tool {
	out := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		// No server-initiated messages, so there is nothing to stream. The
		// spec allows refusing the SSE channel outright.
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP server is POST-only (no SSE channel)", http.StatusMethodNotAllowed)
		return
	}

	// The proxy strips any client-supplied value and sets this only after it
	// has authenticated someone, so whatever arrives here came from the proxy.
	// It is still only ever an audit label — nothing is authorized on it.
	r = r.WithContext(withActor(r.Context(), r.Header.Get(actorHeader)))

	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{errParse, "parse error: " + err.Error()}})
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{errInvalidRequest, "jsonrpc must be \"2.0\""}})
		return
	}

	// A notification has no id and MUST NOT be answered. 202 with no body is
	// what the transport expects.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rerr := s.dispatch(r.Context(), req)
	if rerr != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr})
		return
	}
	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": s.toolList()}, nil

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{errInvalidParams, "bad params: " + err.Error()}
		}
		t, ok := s.tools[p.Name]
		if !ok {
			// Names of unregistered write tools are not leaked here — an
			// unknown name is an unknown name whether or not writes are off.
			return nil, &rpcError{errMethodNotFound, "unknown tool: " + p.Name}
		}
		out, err := t.Handler(ctx, p.Arguments)
		if err != nil {
			// A tool failure is a RESULT with isError, not a protocol error:
			// the model needs to see what went wrong so it can adapt.
			log.Printf("tool %s: %v", p.Name, err)
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
				"isError": true,
			}, nil
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": out}},
		}, nil
	}
	return nil, &rpcError{errMethodNotFound, "unknown method: " + req.Method}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- argument helpers ----

func argString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("argument %q must not be empty", key)
	}
	return s, nil
}

// argOptionalString reads an optional string argument that is allowed to be
// present-but-empty — unlike argString, an empty string is a valid value
// meaning "use the default" (e.g. zone), not an error. Absent entirely also
// means the default; both cases return "". Only a non-string value is
// rejected.
func argOptionalString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return strings.TrimSpace(s), nil
}

// argInt accepts a JSON number (which decodes to float64) and rejects a
// fractional value rather than silently truncating a replica count.
func argInt(args map[string]any, key string) (int, error) {
	v, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing required argument %q", key)
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("argument %q must be a number", key)
	}
	if f != float64(int(f)) {
		return 0, fmt.Errorf("argument %q must be a whole number", key)
	}
	return int(f), nil
}

func argBool(args map[string]any, key string) (bool, error) {
	v, ok := args[key]
	if !ok {
		return false, fmt.Errorf("missing required argument %q", key)
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("argument %q must be a boolean", key)
	}
	return b, nil
}

// argEnvEdits reads the optional "<key>" object argument: a map of env-var
// edits merged onto the service's current env (see serviceenv.go). Keys must
// be non-empty and free of "=" — an embedded "=" would corrupt the KEY=VALUE
// encoding mergeEnv writes and, once re-split, would silently rename the
// variable and mangle its value.
func argEnvEdits(args map[string]any, key string) (map[string]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an object", key)
	}
	out := make(map[string]string, len(m))
	for rawKey, raw := range m {
		// Trim like argString does for scalar identifiers — env var names are
		// conventionally bare identifiers, and an untrimmed key silently fails
		// to match the real (trimmed) key mergeEnv indexes by, appending a
		// bogus new var instead of updating the intended one.
		k := strings.TrimSpace(rawKey)
		if k == "" {
			return nil, fmt.Errorf("argument %q: env var name must not be empty", key)
		}
		if strings.Contains(k, "=") {
			return nil, fmt.Errorf("argument %q: env var name %q must not contain \"=\"", key, k)
		}
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q: value for %q must be a string", key, k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("argument %q: env var name %q given more than once (after trimming whitespace)", key, k)
		}
		out[k] = s
	}
	return out, nil
}

// argStringSlice reads an optional array-valued argument whose items must
// all be strings (e.g. env_ack: the keys a caller has chosen to overwrite).
func argStringSlice(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an array", key)
	}
	out := make([]string, 0, len(arr))
	for i, raw := range arr {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q[%d] must be a string", key, i)
		}
		out = append(out, s)
	}
	return out, nil
}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

type actorKey struct{}

func withActor(ctx context.Context, assertion string) context.Context {
	if assertion == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey{}, assertion)
}

func actorFrom(ctx context.Context) string {
	v, _ := ctx.Value(actorKey{}).(string)
	return v
}

// serveMCP starts the MCP listener. Mounted at "/" because the proxy strips the
// /mcp/dashboard prefix before forwarding; registering the prefix here too
// would double it.
func serveMCP(addr string, s *Server, allowWrites bool) {
	mux := http.NewServeMux()
	mux.Handle("/", s)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ListenAndServe() }()

	mode := "read-only"
	if allowWrites {
		mode = "READ-WRITE (MCP_ALLOW_WRITES set)"
	}
	log.Printf("mcp on %s — %d tools, %s", addr, len(s.tools), mode)
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
