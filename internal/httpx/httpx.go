// Package httpx holds tiny HTTP helpers shared by the dashboard, monitor,
// proxy, and edge binaries. Add things here only when a pattern is genuinely
// duplicated across binaries — don't pre-extract.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as JSON with the given status. The underlying encoder
// error is intentionally swallowed — by the time the body is half-written,
// the response is already going out and there's nothing useful to do.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteErr returns a 500 JSON {"error": "..."} body. Use for unexpected
// internal failures; for client errors prefer http.Error with a specific code.
func WriteErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
