package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// withAccessLog writes one JSONL line per request to stdout.
// Captures: timestamp, method, host, path, status, bytes, duration, client IP.

func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &logResponseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(lw, r)
		fmt.Fprintf(os.Stdout,
			`{"ts":%q,"ip":%q,"method":%q,"host":%q,"path":%q,"status":%d,"bytes":%d,"ms":%d}`+"\n",
			start.UTC().Format(time.RFC3339),
			remoteIP(r), r.Method, r.Host, r.URL.RequestURI(),
			lw.status, lw.bytes, time.Since(start).Milliseconds(),
		)
	})
}

type logResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (l *logResponseWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}
func (l *logResponseWriter) Write(b []byte) (int, error) {
	n, err := l.ResponseWriter.Write(b)
	l.bytes += n
	return n, err
}

// withForwardedHeaders overwrites X-Real-IP / X-Forwarded-* with values
// derived from the real TCP peer. We are the outermost hop — any inbound
// X-Forwarded-* is attacker-supplied, so we do not preserve or append to it.
// Downstream services can then trust XFF/XFP/XFH as authoritative.
func withForwardedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r)
		r.Header.Set("X-Real-IP", ip)
		r.Header.Set("X-Forwarded-For", ip)
		if r.TLS != nil {
			r.Header.Set("X-Forwarded-Proto", "https")
		} else {
			r.Header.Set("X-Forwarded-Proto", "http")
		}
		next.ServeHTTP(w, r)
	})
}

// withHSTS sets Strict-Transport-Security on TLS responses. Once a browser
// has seen this header it refuses plaintext to the domain for the max-age
// window, defeating first-visit downgrade attacks after the first hit.
func withHSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
