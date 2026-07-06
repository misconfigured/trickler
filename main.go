package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/alexcesaro/statsd"
	"github.com/brianvoe/gofakeit"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Endpoints []EndpointConfig `yaml:"endpoints"`
}

type EndpointConfig struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	// Frequency = legacy mode: one request every N SECONDS (min 1s). Kept for
	// backward compatibility; IGNORED when Rate > 0.
	Frequency int `yaml:"frequency"`
	// Rate = requests per SECOND (sub-second capable). When > 0 this overrides
	// Frequency and drives an async, worker-bounded send loop so a slow response
	// never throttles the send rate (#175 load testing).
	Rate float64 `yaml:"rate"`
	// Workers = max concurrent in-flight requests for this endpoint (default 512).
	Workers int `yaml:"workers"`
}

var log = logrus.New()
var statsdClient *statsd.Client

// A connection-pooling client so high request rates aren't bottlenecked on TCP/TLS
// setup (the biggest cap on throughput). http.DefaultClient has no tuned transport.
// Assigned in main() so the connection max-lifetime (which forces periodic DNS
// re-resolution — see lifetimeDialer) is read from the environment.
var httpClient *http.Client

// buildHTTPClient wires the tuned transport + the connection max-lifetime dialer.
// TRICKLER_CONN_MAX_LIFETIME_SECONDS (default 15s) caps connection age so the
// keepalive pool periodically re-dials and RE-RESOLVES DNS — otherwise Go reuses
// live connections forever and the generator ignores geo/load-shed DNS changes.
// Set it to 0 to disable (pin forever, the old behavior).
func buildHTTPClient() *http.Client {
	maxLife := 15 * time.Second
	if v := os.Getenv("TRICKLER_CONN_MAX_LIFETIME_SECONDS"); v != "" {
		if n, err := time.ParseDuration(v + "s"); err == nil {
			maxLife = n
		}
	}
	dialer := &lifetimeDialer{
		d:       net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		maxLife: maxLife,
	}
	log.Infof("http client: conn max-lifetime %s (re-resolves DNS to honor load-shedding)", maxLife)
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConns:        2000,
			MaxIdleConnsPerHost: 1024,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}

// Counters for the periodic achieved-rate reporter (#175).
var (
	cntSent    uint64
	cntOK      uint64
	cntFailed  uint64
	cntDropped uint64
)

func incr(name string) {
	if statsdClient != nil {
		statsdClient.Increment(name)
	}
}

func init() {
	log.Formatter = &logrus.TextFormatter{
		FullTimestamp: true,
	}
	level, err := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		log.SetLevel(logrus.InfoLevel)
	} else {
		log.SetLevel(level)
	}
}

func initStatsd() {
	statsdHost := os.Getenv("STATSD_HOST")
	if statsdHost == "" {
		statsdHost = "127.0.0.1"
		log.Println("STATSD_HOST not provided; defaulting to", statsdHost)
	}
	statsdPort := os.Getenv("STATSD_PORT")
	if statsdPort == "" {
		statsdPort = "8125"
		log.Println("STATSD_PORT not provided; defaulting to", statsdPort)
	}
	statsdAddress := fmt.Sprintf("%s:%s", statsdHost, statsdPort)
	var err error
	statsdClient, err = statsd.New(statsd.Address(statsdAddress), statsd.Prefix("trickler"))
	if err != nil {
		log.Printf("Warning: Failed to create StatsD client: %s", err)
		statsdClient = nil
	} else {
		log.Println("StatsD client created successfully")
	}
}

func main() {
	healthCheck := flag.Bool("health", false, "perform a health check")
	flag.Parse()

	if *healthCheck {
		// Perform health checks
		if performHealthChecks() {
			fmt.Println("Health check passed")
			os.Exit(0)
		} else {
			fmt.Println("Health check failed")
			os.Exit(1)
		}
	}

	initStatsd()

	httpClient = buildHTTPClient()
	if shutdown, err := initOTel(context.Background()); err != nil {
		log.Warnf("OTLP metrics init failed: %s (continuing without client metrics)", err)
	} else {
		defer func() { _ = shutdown(context.Background()) }()
	}

	configPath := "config/config.yaml"
	config, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading config: %s", err)
		os.Exit(1)
	}

	go reportLoop()
	for _, endpoint := range config.Endpoints {
		go handleEndpoint(endpoint)
	}

	select {}
}

func loadConfig(configPath string) (*Config, error) {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Printf("Failed to read config file: %s", err)
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		log.Printf("Failed to unmarshal config data: %s", err)
		return nil, err
	}

	replaceEnvVars(&config)

	for _, endpoint := range config.Endpoints {
		log.Printf("Loaded endpoint: URL=%s Method=%s rate=%.1f/s frequency=%ds", endpoint.URL, endpoint.Method, endpoint.Rate, endpoint.Frequency)
	}
	return &config, nil
}

// Add the template variables you require from https://github.com/brianvoe/gofakeit?tab=readme-ov-file#functions
func generatePayload(templatePath string) (string, error) {
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return "", err
	}

	startDate := time.Now().AddDate(0, 0, 1)
	endDate := time.Now().AddDate(0, 0, 180)

	data := struct {
		FirstName  string
		LastName   string
		Email      string
		MoveInDate time.Time
	}{
		FirstName:  gofakeit.FirstName(),
		LastName:   gofakeit.LastName(),
		Email:      gofakeit.Email(),
		MoveInDate: gofakeit.DateRange(startDate, endDate),
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func handleEndpoint(endpoint EndpointConfig) {
	// Rate mode (#175): requests/sec, sub-second capable. Dispatch each request
	// ASYNC (bounded by Workers) so a slow response never throttles the send rate —
	// the legacy synchronous send capped one endpoint at ~1/latency.
	if endpoint.Rate > 0 {
		interval := time.Duration(float64(time.Second) / endpoint.Rate)
		if interval < time.Microsecond {
			interval = time.Microsecond
		}
		workers := endpoint.Workers
		if workers <= 0 {
			workers = 512
		}
		sem := make(chan struct{}, workers)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		log.Infof("endpoint %s: rate=%.1f req/s (interval %s, workers %d)", endpoint.URL, endpoint.Rate, interval, workers)
		for range ticker.C {
			select {
			case sem <- struct{}{}:
				go func() {
					defer func() { <-sem }()
					sendOne(endpoint)
				}()
			default:
				// every worker busy → the target can't keep up at this rate.
				atomic.AddUint64(&cntDropped, 1)
				incr("endpoint.dropped")
			}
		}
		return
	}

	// Legacy frequency mode: one request every N seconds.
	if endpoint.Frequency <= 0 {
		log.Warnf("endpoint %s has neither rate nor a positive frequency; skipping", endpoint.URL)
		return
	}
	ticker := time.NewTicker(time.Duration(endpoint.Frequency) * time.Second)
	defer ticker.Stop()
	log.Infof("endpoint %s: frequency=%ds", endpoint.URL, endpoint.Frequency)
	for range ticker.C {
		sendOne(endpoint)
	}
}

// sendOne builds, sends, and records one request. Safe to call concurrently.
func sendOne(endpoint EndpointConfig) {
	atomic.AddUint64(&cntSent, 1)
	var bodyReader io.Reader
	if endpoint.Body != "" {
		payload, err := generatePayload(endpoint.Body)
		if err != nil {
			log.Errorf("payload gen for %s failed: %s", endpoint.URL, err)
			atomic.AddUint64(&cntFailed, 1)
			incr("endpoint.failure")
			return
		}
		bodyReader = strings.NewReader(payload)
	}

	req, err := http.NewRequest(endpoint.Method, endpoint.URL, bodyReader)
	if err != nil {
		log.Errorf("request build for %s failed: %s", endpoint.URL, err)
		atomic.AddUint64(&cntFailed, 1)
		incr("endpoint.failure")
		return
	}
	for key, value := range endpoint.Headers {
		req.Header.Set(key, value)
	}

	// Capture the per-phase connection timing from the CLIENT's view (httptrace),
	// so SigNoz can show DNS / connect / TLS / TTFB latency PER EDGE IP as it
	// overloads — the real "is the server degrading" signal.
	var t reqTiming
	var dnsStart, connStart, tlsStart, start time.Time
	trace := &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:      func(httptrace.DNSDoneInfo) { t.dns = time.Since(dnsStart) },
		ConnectStart: func(_, _ string) { connStart = time.Now() },
		ConnectDone:  func(_, _ string, _ error) { t.connect = time.Since(connStart) },
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { t.tls = time.Since(tlsStart) },
		GotConn: func(info httptrace.GotConnInfo) {
			t.reused = info.Reused
			if info.Conn != nil {
				t.peerIP = hostOnly(info.Conn.RemoteAddr().String())
			}
		},
		GotFirstResponseByte: func() { t.ttfb = time.Since(start) },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	start = time.Now()
	response, err := httpClient.Do(req)
	if err != nil {
		t.total = time.Since(start)
		t.record(endpoint, 0, "err")
		log.Errorf("request to %s failed: %s", endpoint.URL, err)
		atomic.AddUint64(&cntFailed, 1)
		incr("endpoint.failure")
		return
	}
	// Drain + close so the connection is reused (keepalive) — essential for rate.
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	t.total = time.Since(start)
	t.record(endpoint, response.StatusCode, classOf(response.StatusCode))

	atomic.AddUint64(&cntOK, 1)
	incr("endpoint.success")
	if statsdClient != nil {
		statsdClient.Gauge("endpoint.response", float64(response.StatusCode))
		statsdClient.Timing("endpoint.latency", int(t.total.Milliseconds()))
	}
	log.Debugf("%s -> %s (%dms ttfb=%dms edge=%s reused=%t)", endpoint.URL, response.Status,
		t.total.Milliseconds(), t.ttfb.Milliseconds(), t.peerIP, t.reused)
}

// reportLoop logs the achieved send rate + outcome deltas every 5s, so a high-rate
// run is legible at Info level without a log line per request.
func reportLoop() {
	var pSent, pOK, pFail, pDrop uint64
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		s := atomic.LoadUint64(&cntSent)
		ok := atomic.LoadUint64(&cntOK)
		f := atomic.LoadUint64(&cntFailed)
		d := atomic.LoadUint64(&cntDropped)
		log.Infof("trickle: %.0f req/s (ok=%d fail=%d dropped=%d /5s; totals sent=%d ok=%d fail=%d)",
			float64(s-pSent)/5.0, ok-pOK, f-pFail, d-pDrop, s, ok, f)
		pSent, pOK, pFail, pDrop = s, ok, f, d
	}
}

func replaceEnvVars(config *Config) {
	for i, endpoint := range config.Endpoints {
		for key, value := range endpoint.Headers {
			if strings.Contains(value, "{") && strings.Contains(value, "}") {
				start := strings.Index(value, "{") + 1
				end := strings.Index(value, "}")
				envVar := value[start:end]
				realValue := os.Getenv(envVar)
				if realValue == "" {
					log.Printf("Environment variable %s not set", envVar)
				}
				config.Endpoints[i].Headers[key] = strings.Replace(value, "{"+envVar+"}", realValue, 1)
			}
		}
	}
}

func performHealthChecks() bool {
	// TODO: Add some actual health checks here like ensuring that the configuration and templates render
	return true
}
