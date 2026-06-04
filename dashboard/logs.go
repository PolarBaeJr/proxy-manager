package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// containerSummary is the lightweight shape the Logs picker consumes — just
// enough to label the dropdown without leaking labels/env to the client.
type containerSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Service string `json:"service,omitempty"`
}

func (c *dockerClient) listContainerSummaries(ctx context.Context) ([]containerSummary, error) {
	all, err := c.listAll(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]containerSummary, 0, len(all))
	for _, ct := range all {
		out = append(out, containerSummary{
			ID:      ct.ID,
			Name:    ct.name(),
			Image:   ct.Image,
			State:   ct.State,
			Service: ct.Labels[labelService],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// logLine is one demultiplexed line from a container's stdout/stderr stream.
type logLine struct {
	Stream string `json:"stream"` // "stdout" | "stderr"
	Text   string `json:"text"`
}

// containerLogs fetches up to `tail` lines from a container. The Docker daemon
// returns a multiplexed framed stream when TTY is off (header: type|0|0|0|len4),
// and raw bytes when TTY is on — we sniff and handle both.
func (c *dockerClient) containerLogs(ctx context.Context, idOrName string, tail int) ([]logLine, error) {
	if tail <= 0 {
		tail = 200
	}
	if tail > 5000 {
		tail = 5000
	}
	q := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&timestamps=0&tail=%d",
		idOrName, tail)
	body, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 4<<20)) // 4MB cap
	if err != nil {
		return nil, err
	}
	return parseDockerLogStream(data), nil
}

// parseDockerLogStream demultiplexes Docker's frame format. If the first byte
// isn't 1/2 (stdout/stderr) or the framing doesn't add up, it falls back to
// treating the whole payload as raw text (TTY mode).
func parseDockerLogStream(data []byte) []logLine {
	out := []logLine{}
	if len(data) == 0 {
		return out
	}
	// Heuristic: framed streams always start with 0x01 or 0x02; raw TTY rarely does.
	if data[0] != 0x01 && data[0] != 0x02 {
		return splitLines(data, "stdout")
	}
	for len(data) >= 8 {
		stream := "stdout"
		if data[0] == 0x02 {
			stream = "stderr"
		}
		size := int(binary.BigEndian.Uint32(data[4:8]))
		if size < 0 || size > len(data)-8 {
			// Frame doesn't fit — treat the rest as raw and bail.
			out = append(out, splitLines(data, stream)...)
			break
		}
		chunk := data[8 : 8+size]
		out = append(out, splitLines(chunk, stream)...)
		data = data[8+size:]
	}
	return out
}

// validContainerName matches Docker's allowed character set: [a-zA-Z0-9][a-zA-Z0-9_.-]+
// or a 64-hex container ID. Blocks any path-traversal or URL trickery.
func validContainerName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			continue
		case i > 0 && (r == '_' || r == '.' || r == '-'):
			continue
		default:
			return false
		}
	}
	return true
}

func splitLines(data []byte, stream string) []logLine {
	out := []logLine{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		out = append(out, logLine{Stream: stream, Text: line})
	}
	return out
}

// registerLogRoutes wires the Logs endpoints onto the dashboard mux. Auth-gated
// (read-only — no write surface here), so token-auth and session-auth both work.
func registerLogRoutes(mux *http.ServeMux, dc *dockerClient, auth *AuthStore) {
	mux.HandleFunc("/api/logs/containers", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		list, err := dc.listContainerSummaries(req.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))

	mux.HandleFunc("/api/logs/", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/api/logs/")
		if name == "" || name == "containers" || !validContainerName(name) {
			http.NotFound(w, req)
			return
		}
		tail := 200
		if t := req.URL.Query().Get("tail"); t != "" {
			if n, err := strconv.Atoi(t); err == nil {
				tail = n
			}
		}
		lines, err := dc.containerLogs(req.Context(), name, tail)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"container": name,
			"tail":      tail,
			"lines":     lines,
		})
	}))
}

