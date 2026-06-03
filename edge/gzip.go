package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzip middleware. Wraps response writer; encodes if the client supports it
// AND the response is a compressible type (text/json/javascript/etc).

var gzPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := gzPool.Get().(*gzip.Writer)
		defer gzPool.Put(gw)
		gw.Reset(w)
		defer gw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, w: gw}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	w           io.Writer
	wroteHeader bool
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader && g.Header().Get("Content-Type") == "" {
		g.Header().Set("Content-Type", http.DetectContentType(b))
	}
	return g.w.Write(b)
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.wroteHeader = true
	g.ResponseWriter.WriteHeader(code)
}
