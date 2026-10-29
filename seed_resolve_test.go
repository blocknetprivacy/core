package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestResolveSeedNodesUsesPinnedPeerIDs(t *testing.T) {
	const pinned = "12D3KooWSQTy8rav5nmapgxomAMpSrigJTUXHmjH25dtHhGU3BAM"
	const other = "12D3KooWQLYYe5jVgeQ8qrqtZitD8r7HmdPvyM2n4RQ6wRMeZuk7"

	cases := []struct {
		name     string
		reported string
		want     []string
	}{
		{"matching peer ID", pinned, []string{"/ip4/127.0.0.1/tcp/28080/p2p/" + pinned}},
		{"swapped peer ID", other, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.reported))
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			port, _ := strconv.Atoi(u.Port())

			got := ResolveSeedNodes([]SeedHost{{Host: "127.0.0.1", PeerID: pinned}}, 28080, port)
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
