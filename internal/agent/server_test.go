package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/agent"
	"github.com/yaojingang/geoflow-updater/internal/doctor"
)

type statusProvider struct{}

func (statusProvider) Run(context.Context, string) doctor.Report {
	return doctor.Report{SchemaVersion: 1, Status: doctor.StatusPass}
}

func TestHealthEndpointIsAvailableWithoutControlCredential(t *testing.T) {
	t.Parallel()

	handler := agent.Server{Version: "0.1.0", Status: statusProvider{}}.Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["status"] != "ok" || payload["version"] != "0.1.0" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestInstanceStatusRequiresItsControlCredential(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	if err := os.MkdirAll(instanceDir, 0o750); err != nil {
		t.Fatalf("mkdir instance: %v", err)
	}
	token := strings.Repeat("a", 43)
	if err := os.WriteFile(filepath.Join(instanceDir, "control.token"), []byte(token+"\n"), 0o640); err != nil {
		t.Fatalf("write token: %v", err)
	}

	handler := agent.Server{StateDir: stateDir, Version: "0.1.0", Status: statusProvider{}}.Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/instances/primary/status", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/instances/primary/status", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode authorized response: %v", err)
	}
	if payload["updater_version"] != "0.1.0" || payload["schema_version"] != float64(1) {
		t.Fatalf("authorized payload = %#v", payload)
	}
}

func TestAgentRejectsUnknownOperations(t *testing.T) {
	t.Parallel()

	handler := agent.Server{Version: "0.1.0", Status: statusProvider{}}.Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/health", nil))

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}
