package main

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"text/template"
	"time"

	"gopkg.in/yaml.v2"
)

func TestConfigLoad(t *testing.T) {
	filename := filepath.Join("config", "config.yaml")
	yamlFile, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Fatalf("Failed to read %s: %s", filename, err)
	}

	var config Config
	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		t.Fatalf("Unmarshal: %s", err)
	}

	if len(config.Endpoints) == 0 {
		t.Error("No endpoints found in the configuration")
	}

	if len(config.Endpoints) > 0 {
		endpoint := config.Endpoints[0]
		if endpoint.URL == "" || endpoint.Method == "" {
			t.Error("URL or Method for the first endpoint is not specified")
		}
	}
}

func TestTemplateParsing(t *testing.T) {
	templateFile := filepath.Join("templates", "partner.json")
	tmpl, err := template.New("partner.json").ParseFiles(templateFile)
	if err != nil {
		t.Fatalf("Failed to parse template %s: %s", templateFile, err)
	}

	data := struct {
		FirstName  string
		LastName   string
		Email      string
		MoveInDate time.Time
	}{
		FirstName:  "John",
		LastName:   "Doe",
		Email:      "john.doe@example.com",
		MoveInDate: time.Now(),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("Failed to execute template: %s", err)
	}

	if buf.Len() == 0 {
		t.Error("Template rendered an empty string")
	}
}

func TestInitStatsd(t *testing.T) {
	os.Setenv("STATSD_HOST", "127.0.0.1")
	os.Setenv("STATSD_PORT", "8125")

	initStatsd()

	if statsdClient == nil {
		t.Error("StatsD client should not be nil")
	}
}

func TestLoadConfig(t *testing.T) {
	config, err := loadConfig("config/config.yaml")
	if err != nil {
		t.Fatalf("Failed to load configuration: %s", err)
	}
	if len(config.Endpoints) == 0 {
		t.Error("Expected non-zero endpoints in configuration")
	}
}

func TestGeneratePayload(t *testing.T) {
	payload, err := generatePayload("templates/partner.json")
	if err != nil {
		t.Errorf("Error generating payload: %s", err)
	}
	if payload == "" {
		t.Error("Generated payload should not be empty")
	}
}

func TestHandleEndpoint(t *testing.T) {
	// Setup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	endpointConfig := EndpointConfig{
		URL:       server.URL,
		Method:    "GET",
		Headers:   map[string]string{"Content-Type": "application/json"},
		Body:      "templates/partner.json",
		Frequency: 1,
	}

	go handleEndpoint(endpointConfig)
	time.Sleep(4 * time.Second)

}

func TestReplaceEnvVars(t *testing.T) {
	os.Setenv("TEST_API_KEY", "abc123")
	config := &Config{
		Endpoints: []EndpointConfig{
			{Headers: map[string]string{"Authorization": "Bearer {TEST_API_KEY}"}},
		},
	}

	replaceEnvVars(config)
	if config.Endpoints[0].Headers["Authorization"] != "Bearer abc123" {
		t.Errorf("Expected header to be 'Bearer abc123', got '%s'", config.Endpoints[0].Headers["Authorization"])
	}
}

func TestPerformHealthChecks(t *testing.T) {
	if !performHealthChecks() {
		t.Error("Health checks failed")
	}
}
