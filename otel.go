package main

import (
	"context"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Client-side connection-phase histograms (milliseconds). Recording from the
// CLIENT is the point: server CPU says a box is busy, but DNS/connect/TLS/TTFB
// latency says whether it's actually DEGRADING service. Every sample carries the
// edge IP it landed on (net.peer.ip), so SigNoz can chart p95 TTFB PER EDGE and
// show an overloaded node's latency climb — and whether load-shedding moved us
// off it. Milliseconds so the SDK's default histogram buckets (tuned for ms) fit.
var (
	hDNS     metric.Float64Histogram
	hConnect metric.Float64Histogram
	hTLS     metric.Float64Histogram
	hTTFB    metric.Float64Histogram
	hTotal   metric.Float64Histogram
	cReq     metric.Int64Counter
	otelOn   bool
)

// initOTel wires an OTLP/gRPC MeterProvider to the fleet collector when
// OTEL_EXPORTER_OTLP_ENDPOINT is set (e.g. "100.64.0.72:4317", plaintext over the
// tailnet — same collector the edges push to). Returns a shutdown func. When the
// env var is unset, metrics are a no-op so trickler still runs standalone.
func initOTel(ctx context.Context) (func(context.Context) error, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		log.Warn("OTEL_EXPORTER_OTLP_ENDPOINT unset; client metrics disabled")
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(), // plaintext to the private mesh collector
	)
	if err != nil {
		return nil, err
	}

	host, _ := os.Hostname()
	if h := os.Getenv("OTEL_HOST_NAME"); h != "" {
		host = h // host.name is flaky from the OS; let the fleet pattern override it
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", "trickler"),
		attribute.String("host.name", host),
		attribute.String("region", envOr("TRICKLER_REGION", "sjc")),
	))
	if err != nil {
		return nil, err
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(10*time.Second))),
	)
	otel.SetMeterProvider(mp)

	m := mp.Meter("trickler")
	hDNS, _ = m.Float64Histogram("trickler.dns.duration", metric.WithUnit("ms"),
		metric.WithDescription("DNS resolution time"))
	hConnect, _ = m.Float64Histogram("trickler.connect.duration", metric.WithUnit("ms"),
		metric.WithDescription("TCP connect time"))
	hTLS, _ = m.Float64Histogram("trickler.tls.duration", metric.WithUnit("ms"),
		metric.WithDescription("TLS handshake time"))
	hTTFB, _ = m.Float64Histogram("trickler.ttfb", metric.WithUnit("ms"),
		metric.WithDescription("time to first response byte (server processing latency)"))
	hTotal, _ = m.Float64Histogram("trickler.request.duration", metric.WithUnit("ms"),
		metric.WithDescription("total request time"))
	cReq, _ = m.Int64Counter("trickler.requests",
		metric.WithDescription("requests by outcome / edge"))
	otelOn = true
	log.Infof("OTLP metrics -> %s (service=trickler host=%s region=%s)",
		endpoint, host, envOr("TRICKLER_REGION", "sjc"))
	return mp.Shutdown, nil
}

// reqTiming is the per-request phase breakdown captured via httptrace.
type reqTiming struct {
	dns, connect, tls, ttfb, total time.Duration
	peerIP                         string // the EDGE IP this connection used
	reused                         bool   // keepalive reuse (skips DNS+connect+TLS)
	proto                          string // h1 | h2 | h3
}

// record emits the phase histograms + a request counter, dimensioned so SigNoz can
// slice by edge IP, status, and keepalive reuse.
func (t reqTiming) record(ep EndpointConfig, code int, class string) {
	if !otelOn {
		return
	}
	ctx := context.Background()
	attrs := metric.WithAttributes(
		attribute.String("http.host", hostOf(ep.URL)),
		attribute.String("http.method", methodOr(ep.Method)),
		attribute.Int("http.status_code", code),
		attribute.String("status_class", class),
		attribute.String("net.peer.ip", t.peerIP),
		attribute.Bool("conn.reused", t.reused),
		attribute.String("protocol", t.proto),
	)
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	// DNS/connect/TLS are only non-zero on a FRESH connection; a reused keepalive
	// conn records just TTFB + total (which is exactly the signal that matters).
	if t.dns > 0 {
		hDNS.Record(ctx, ms(t.dns), attrs)
	}
	if t.connect > 0 {
		hConnect.Record(ctx, ms(t.connect), attrs)
	}
	if t.tls > 0 {
		hTLS.Record(ctx, ms(t.tls), attrs)
	}
	if t.ttfb > 0 {
		hTTFB.Record(ctx, ms(t.ttfb), attrs)
	}
	hTotal.Record(ctx, ms(t.total), attrs)
	cReq.Add(ctx, 1, attrs)
}

// --- connection max-lifetime: honor DNS load-shedding -----------------------

// lifetimeDialer caps how long any one connection can live. Go's Transport reuses
// live keepalive connections WITHOUT re-resolving DNS, so under continuous load a
// generator pins to whatever edge it first resolved — ignoring geo/load-shed
// changes. Retiring connections on a timer forces periodic re-dial, which
// re-resolves DNS and lets the control plane actually steer us off a hot edge.
type lifetimeDialer struct {
	d       net.Dialer
	maxLife time.Duration
}

func (l *lifetimeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := l.d.DialContext(ctx, network, addr)
	if err != nil || l.maxLife <= 0 {
		return c, err
	}
	// Jitter each connection's lifetime across [maxLife/2, 3*maxLife/2). Without
	// this, connections created together (e.g. at startup) all expire at once — a
	// synchronized reconnect wave that craters throughput every maxLife seconds.
	// Spreading retirements keeps re-resolution steady with no throughput dip.
	life := l.maxLife/2 + time.Duration(rand.Int64N(int64(l.maxLife)))
	return &deadlineConn{Conn: c, expiry: time.Now().Add(life)}, nil
}

// deadlineConn fails reads once past its expiry, so the Transport discards it and
// dials a fresh connection (idempotent GETs are retried transparently).
type deadlineConn struct {
	net.Conn
	expiry time.Time
}

type retiredConnErr struct{}

func (retiredConnErr) Error() string   { return "trickler: connection retired (max-lifetime; re-resolving)" }
func (retiredConnErr) Timeout() bool   { return false }
func (retiredConnErr) Temporary() bool { return false }

func (c *deadlineConn) Read(b []byte) (int, error) {
	if time.Now().After(c.expiry) {
		return 0, retiredConnErr{}
	}
	return c.Conn.Read(b)
}

// --- small helpers ----------------------------------------------------------

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func methodOr(m string) string {
	if m == "" {
		return "GET"
	}
	return m
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return raw
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func classOf(code int) string {
	switch {
	case code == 0:
		return "err"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
