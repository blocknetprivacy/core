package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

// explorerImmutableCache is applied to pages whose content can no longer
// change: a block buried deeper than MaxReorgDepth is final, so its page and
// the pages of its transactions are safe to cache indefinitely. Everything else
// stays no-store, set by Explorer.ServeHTTP.
const explorerImmutableCache = "public, max-age=31536000, immutable"

// setImmutableCacheIfFinal marks a response cacheable once the block it renders
// is past hard finality. Live values on such a page (confirmations, chain tip)
// are corrected by the event stream, so a cached copy is not a stale copy.
func setImmutableCacheIfFinal(w http.ResponseWriter, blockHeight, chainHeight uint64) {
	if chainHeight < blockHeight {
		return
	}
	if chainHeight-blockHeight > uint64(MaxReorgDepth) {
		w.Header().Set("Cache-Control", explorerImmutableCache)
	}
}

// gzipMiddleware compresses explorer HTML. The event stream is excluded: it is
// long-lived and incrementally flushed, and a compressing wrapper would both
// buffer it and hide the http.Flusher the handler needs.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")

		gz := gzip.NewWriter(w)
		defer gz.Close()

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, writer: gz}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.writer.Write(b)
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Flush() {
	w.writer.Flush()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

var _ io.Writer = (*gzipResponseWriter)(nil)
