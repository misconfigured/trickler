package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
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
	URL       string            `yaml:"url"`
	Method    string            `yaml:"method"`
	Headers   map[string]string `yaml:"headers"`
	Body      string            `yaml:"body"`
	Frequency int               `yaml:"frequency"`
}

var log = logrus.New()
var statsdClient *statsd.Client

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

	configPath := "config/config.yaml"
	config, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading config: %s", err)
		os.Exit(1)
	}

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
		log.Printf("Loaded endpoint: URL=%s, Method=%s, Frequency=%ds", endpoint.URL, endpoint.Method, endpoint.Frequency)
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
	if endpoint.Frequency <= 0 {
		log.Warnf("Invalid frequency for endpoint %s; must be greater than zero", endpoint.URL)
		return
	}

	ticker := time.NewTicker(time.Duration(endpoint.Frequency) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		payload, err := generatePayload(endpoint.Body)
		if err != nil {
			log.Errorf("Failed to generate payload for %s: %s", endpoint.URL, err)
			continue
		}

		log.Debugf("Sending payload to %s: %s", endpoint.URL, payload)

		req, err := http.NewRequest(endpoint.Method, endpoint.URL, strings.NewReader(payload))
		if err != nil {
			log.Errorf("Failed to create request for %s: %s", endpoint.URL, err)
			statsdClient.Increment("endpoint.failure")
			continue
		}

		for key, value := range endpoint.Headers {
			req.Header.Set(key, value)
		}

		response, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Errorf("Request to %s failed: %s", endpoint.URL, err)
			continue
		}
		log.Infof("Request to %s Response Status: %s", endpoint.URL, response.Status)
		statsdClient.Increment("endpoint.success")

		statsdClient.Gauge("endpoint.response", float64(response.StatusCode))

		response.Body.Close()
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
