package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/operation"
)

func TestCoordinationRejectsAmbiguousJSON(t *testing.T) {
	valid := `{"action":"backup","plan_id":"` + strings.Repeat("a", 32) + `","plan_sha256":"` + strings.Repeat("b", 64) + `","expected_epoch":"` + strings.Repeat("c", 32) + `","allow_maintenance":true,"confirm_host_access":true,"client_request_id":"request-0001","scope":"updater:backup","actor":{"management_instance_id":"12345678-1234-4234-8234-123456789012","admin_id":1,"identity_sha256":"` + strings.Repeat("d", 64) + `"}}`
	for _, mutate := range []func(string) string{func(s string) string { return strings.Replace(s, `"allow_maintenance":true,`, "", 1) }, func(s string) string {
		return strings.Replace(s, `"allow_maintenance":true`, `"allow_maintenance":null`, 1)
	}, func(s string) string {
		return strings.Replace(s, `"action":"backup"`, `"action":"restore","action":"backup"`, 1)
	}, func(s string) string { return strings.Replace(s, `"admin_id":1`, `"admin_id":2,"admin_id":1`, 1) }, func(s string) string { return s + `{}` }, func(s string) string { return strings.Replace(s, `"scope":`, `"otp":"123456","scope":`, 1) }} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(mutate(valid)))
		var payload coordination.SubmitRequest
		if decodeCoordinated(httptest.NewRecorder(), r, &payload) {
			t.Fatal("ambiguous body accepted", mutate(valid))
		}
	}
	var p coordination.SubmitRequest
	if !decodeCoordinated(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(valid)), &p) || !p.Valid() {
		t.Fatal("valid body rejected")
	}
	expected := `{"action":"backup","allow_maintenance":true,"confirm_host_access":true,"expected_epoch":"` + strings.Repeat("c", 32) + `","plan_id":"` + strings.Repeat("a", 32) + `","plan_sha256":"` + strings.Repeat("b", 64) + `"}`
	var canonical map[string]any
	if err := json.Unmarshal([]byte(expected), &canonical); err != nil {
		t.Fatal(err)
	}
	if p.BusinessSHA256() != coordination.Digest(canonical) {
		t.Fatal("canonical business hash mismatch")
	}
}
func TestCoordinationRequest404IsExactAndReadsHaveNoSideEffects(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "instances", "primary")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 43)
	if err := os.WriteFile(filepath.Join(dir, "control.token"), []byte(token), 0640); err != nil {
		t.Fatal(err)
	}
	manager := &operation.Manager{StateDir: state}
	server := Server{StateDir: state, Operations: manager}
	handler := server.Handler()
	// The read endpoints require the local control token, even when the request does not exist.
	for _, auth := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "/v2/instances/primary/requests/request-0001", nil)
		if auth {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		expected := http.StatusUnauthorized
		if auth {
			expected = http.StatusNotFound
		}
		if response.Code != expected {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		if auth && !bytes.Contains(response.Body.Bytes(), []byte(`"error":"request_not_found"`)) {
			t.Fatal(response.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "coordination")); !os.IsNotExist(err) {
		t.Fatal("poll created coordination state", err)
	}
}
