package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blocknet/p2p"
)

const (
	MainnetP2PPort    = 28080
	MainnetPeerIDPort = 28081
	TestnetP2PPort    = 38080
	TestnetPeerIDPort = 38081
)

var DefaultSeedHosts = []string{
	"explorer.blocknetcrypto.com",
	"bnt-0.blocknetcrypto.com",
	"bnt-1.blocknetcrypto.com",
}

// ResolveSeedNodes fetches peer IDs from seed nodes' HTTP endpoints
// and builds full multiaddrs. Accepts IPs or hostnames.
// Fetches run in parallel with a 3s timeout.
func ResolveSeedNodes(hosts []string, p2pPort, peerIDPort int) []string {
	type result struct {
		addr string
	}

	ch := make(chan result, len(hosts))
	client := &http.Client{Timeout: 3 * time.Second}

	for _, host := range hosts {
		go func(host string) {
			url := fmt.Sprintf("http://%s:%d/", host, peerIDPort)
			resp, err := client.Get(url)
			if err != nil {
				ch <- result{}
				return
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				ch <- result{}
				return
			}
			peerID := strings.TrimSpace(string(body))
			if peerID == "" {
				ch <- result{}
				return
			}
			proto := "dns4"
			if net.ParseIP(host) != nil {
				proto = "ip4"
			}
			ch <- result{fmt.Sprintf("/%s/%s/tcp/%d/p2p/%s", proto, host, p2pPort, peerID)}
		}(host)
	}

	var seeds []string
	for range hosts {
		if r := <-ch; r.addr != "" {
			seeds = append(seeds, r.addr)
		}
	}
	return seeds
}

func startPeerIDServer(node *p2p.Node, port int) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, node.PeerID().String())
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Peer ID endpoint failed on port %d: %v", port, err)
		}
	}()

	return server
}

func peerIDPortFromMultiaddr(listenAddr string) int {
	parts := strings.Split(listenAddr, "/")
	for i, p := range parts {
		if p == "tcp" && i+1 < len(parts) {
			if port, err := strconv.Atoi(parts[i+1]); err == nil {
				return port + 1
			}
		}
	}
	return MainnetPeerIDPort
}
