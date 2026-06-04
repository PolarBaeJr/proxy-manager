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
			clientIP(r), r.Method, r.Host, r.URL.RequestURI(),
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

// withForwardedHeaders normalizes X-Real-IP / X-Forwarded-* so upstreams see
// the original client IP + scheme + host even though we've terminated TLS.
func withForwardedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		r.Header.Set("X-Real-IP", ip)
		if existing := r.Header.Get("X-Forwarded-For"); existing == "" {
			r.Header.Set("X-Forwarded-For", ip)
		} else {
			r.Header.Set("X-Forwarded-For", existing+", "+ip)
		}
		if r.TLS != nil {
			r.Header.Set("X-Forwarded-Proto", "https")
		} else {
			r.Header.Set("X-Forwarded-Proto", "http")
		}
		next.ServeHTTP(w, r)
	})
}
