package main

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	explorerProxyTimeout  = 10 * time.Second
	explorerProxyMaxBytes = 8 << 20
)

// explorerProxyClient is shared so proxied requests reuse connections. The
// default client has no timeout at all, which lets a stalled testnet daemon pin
// a mainnet goroutine indefinitely.
var explorerProxyClient = &http.Client{Timeout: explorerProxyTimeout}

func (e *Explorer) proxyRequest(r *http.Request) (*http.Request, error) {
	u := "http://" + e.testnetAddr + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
}

func copyProxyHeaders(dst http.Header, src http.Header) {
	for k, vals := range src {
		// The body is rewritten or streamed, so neither length nor the
		// upstream transfer encoding survives the hop.
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func (e *Explorer) proxyToTestnet(w http.ResponseWriter, r *http.Request) {
	req, err := e.proxyRequest(r)
	if err != nil {
		http.Error(w, "testnet explorer unavailable", http.StatusBadGateway)
		return
	}

	resp, err := explorerProxyClient.Do(req)
	if err != nil {
		http.Error(w, "testnet explorer unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, explorerProxyMaxBytes))
	if err != nil {
		http.Error(w, "testnet explorer read error", http.StatusBadGateway)
		return
	}

	// Inject our egg script so the toggle works even if the testnet
	// daemon is running an older build with stale JS/templates.
	fix := explorerProxyFixScript + "\n" + explorerEggScript
	body = bytes.Replace(body, []byte("</body>"), []byte(fix+"\n</body>"), 1)

	copyProxyHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// proxyStreamToTestnet forwards the event stream without buffering. The
// buffering proxy above would never return on an SSE response, because the
// upstream body only ends when the connection does.
func (e *Explorer) proxyStreamToTestnet(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	req, err := e.proxyRequest(r)
	if err != nil {
		http.Error(w, "testnet explorer unavailable", http.StatusBadGateway)
		return
	}

	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		http.Error(w, "testnet explorer unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyProxyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}
