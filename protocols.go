package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// Per-protocol HTTP clients, built lazily and cached. A load generator should be
// able to drive every protocol the edge serves — HTTP/1.1, HTTP/2, and HTTP/3
// over QUIC — not just h1, since each exercises a different edge path (TLS+ALPN,
// h2 multiplexing, QUIC/UDP) with its own latency + failure profile.
var (
	clientsMu sync.Mutex
	clients   = map[string]*http.Client{}
)

// normalizeProto maps config spellings to h1|h2|h3, defaulting to h1.
func normalizeProto(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "h2", "http2", "http/2":
		return "h2"
	case "h3", "http3", "http/3", "quic":
		return "h3"
	default:
		return "h1"
	}
}

func clientFor(proto string) *http.Client {
	proto = normalizeProto(proto)
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clients[proto]; ok {
		return c
	}
	c := buildClientFor(proto)
	clients[proto] = c
	return c
}

func buildClientFor(proto string) *http.Client {
	switch proto {
	case "h3":
		// HTTP/3 over QUIC (UDP). quic-go negotiates its own transport; the
		// connection-lifetime trick doesn't apply (QUIC manages connection IDs and
		// migration itself), so we just measure it.
		log.Infof("http client[h3]: HTTP/3 over QUIC")
		return &http.Client{Timeout: 15 * time.Second, Transport: &http3.Transport{}}
	case "h2":
		// HTTP/2: ALPN-negotiated over TLS. h2 MULTIPLEXES many streams over few
		// connections, so a per-connection deadline would kill hundreds of streams
		// at once (the oscillation we hit forcing it on h1's path) — use a plain
		// dialer here and let h2 do its thing.
		log.Infof("http client[h2]: HTTP/2 (ALPN), multiplexed")
		return &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        2000,
				MaxIdleConnsPerHost: 256,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	default:
		// HTTP/1.1: the fat independent-connection pool with jittered
		// connection-lifetime (re-resolves DNS to honor load-shedding).
		return buildHTTPClient()
	}
}
