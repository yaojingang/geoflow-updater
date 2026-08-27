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
	"time"

	"github.com/yaojingang/geoflow-updater/internal/agent"
	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/operation"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
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

type operations struct {
	updates   int
	backups   int
	rollbacks []string
	points    []recovery.Point
}

type authorizationCall struct {
	scope authorization.Scope
	code  string
}

type mutationAuthorizer struct {
	calls []authorizationCall
	err   error
}

func (authorizer *mutationAuthorizer) Authorize(_ string, scope authorization.Scope, code string, callback func() error) error {
	authorizer.calls = append(authorizer.calls, authorizationCall{scope: scope, code: code})
	if authorizer.err != nil {
		return authorizer.err
	}

	return callback()
}

func (operations *operations) StartUpdate(instanceID string) (operation.Operation, error) {
	operations.updates++
	return operation.Operation{SchemaVersion: 1, ID: "op-update", InstanceID: instanceID, Kind: operation.KindUpdate, Status: operation.StatusQueued, StartedAt: time.Now()}, nil
}

func (operations *operations) StartBackup(instanceID string) (operation.Operation, error) {
	operations.backups++
	return operation.Operation{SchemaVersion: 1, ID: "op-backup", InstanceID: instanceID}, nil
}

func (operations *operations) StartRollback(instanceID string, recoveryPointID string) (operation.Operation, error) {
	operations.rollbacks = append(operations.rollbacks, recoveryPointID)
	return operation.Operation{SchemaVersion: 1, ID: "op-rollback", InstanceID: instanceID}, nil
}

func (operations *operations) StartVerify(instanceID string) (operation.Operation, error) {
	return operation.Operation{SchemaVersion: 1, ID: "op-verify", InstanceID: instanceID}, nil
}

func (operations *operations) Current(instanceID string) (operation.Operation, error) {
	return operation.Operation{SchemaVersion: 1, ID: "op-current", InstanceID: instanceID}, nil
}

func (operations *operations) RecoveryPoints(instanceID string) ([]recovery.Point, error) {
	if operations.points != nil {
		return operations.points, nil
	}
	return []recovery.Point{{SchemaVersion: 1, ID: "20260827T123456Z-abcdef12", InstanceID: instanceID, Reason: "update-to-2.4.0"}}, nil
}

func TestMutationEndpointsRequireCredentialAndAcceptOnlyTypedOperations(t *testing.T) {
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
	manager := &operations{}
	authorizer := &mutationAuthorizer{}
	handler := agent.Server{StateDir: stateDir, Version: "0.2.0", Status: statusProvider{}, Operations: manager, Authorization: authorizer}.Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/instances/primary/updates", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized update status = %d, want 401", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/updates", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || manager.updates != 0 {
		t.Fatalf("update without mutation authorization status = %d, calls = %d", response.Code, manager.updates)
	}

	authorizedUpdate := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/updates", nil)
	authorizedUpdate.Header.Set("Authorization", "Bearer "+token)
	authorizedUpdate.Header.Set("X-GEOFlow-Updater-Authorization", "123456")
	authorizedUpdateResponse := httptest.NewRecorder()
	handler.ServeHTTP(authorizedUpdateResponse, authorizedUpdate)
	if authorizedUpdateResponse.Code != http.StatusAccepted || manager.updates != 1 {
		t.Fatalf("authorized update status = %d, calls = %d", authorizedUpdateResponse.Code, manager.updates)
	}
	if len(authorizer.calls) != 1 || authorizer.calls[0] != (authorizationCall{scope: authorization.ScopeUpdate, code: "123456"}) {
		t.Fatalf("mutation authorization calls = %#v", authorizer.calls)
	}

	wrongMethod := httptest.NewRequest(http.MethodGet, "/v1/instances/primary/updates", nil)
	wrongMethod.Header.Set("Authorization", "Bearer "+token)
	wrongMethod.Header.Set("X-GEOFlow-Updater-Authorization", "345678")
	wrongMethodResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethodResponse, wrongMethod)
	if wrongMethodResponse.Code != http.StatusMethodNotAllowed || len(authorizer.calls) != 1 {
		t.Fatalf("wrong-method status = %d, mutation authorization calls = %#v", wrongMethodResponse.Code, authorizer.calls)
	}

	updateWithBody := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/updates", strings.NewReader(`{"command":"anything"}`))
	updateWithBody.Header.Set("Authorization", "Bearer "+token)
	updateWithBody.Header.Set("X-GEOFlow-Updater-Authorization", "123456")
	updateWithBodyResponse := httptest.NewRecorder()
	handler.ServeHTTP(updateWithBodyResponse, updateWithBody)
	if updateWithBodyResponse.Code != http.StatusBadRequest || manager.updates != 1 {
		t.Fatalf("update with body status = %d, calls = %d", updateWithBodyResponse.Code, manager.updates)
	}

	rollback := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/rollbacks", strings.NewReader(`{"recovery_point_id":"20260827T123456Z-abcdef12"}`))
	rollback.Header.Set("Authorization", "Bearer "+token)
	rollback.Header.Set("X-GEOFlow-Updater-Authorization", "234567")
	rollbackResponse := httptest.NewRecorder()
	handler.ServeHTTP(rollbackResponse, rollback)
	if rollbackResponse.Code != http.StatusAccepted || len(manager.rollbacks) != 1 {
		t.Fatalf("rollback status = %d, calls = %#v", rollbackResponse.Code, manager.rollbacks)
	}
	if authorizer.calls[len(authorizer.calls)-1].scope != authorization.ScopeRollback {
		t.Fatalf("rollback authorization scope = %q", authorizer.calls[len(authorizer.calls)-1].scope)
	}

	unknown := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/commands", strings.NewReader(`{"command":"rm"}`))
	unknown.Header.Set("Authorization", "Bearer "+token)
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusNotFound {
		t.Fatalf("unknown operation status = %d, want 404", unknownResponse.Code)
	}
}

func TestWebsiteRollbackAcceptsOnlyTheNewestUpdateCheckpoint(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	if err := os.MkdirAll(instanceDir, 0o750); err != nil {
		t.Fatalf("mkdir instance: %v", err)
	}
	token := strings.Repeat("e", 43)
	if err := os.WriteFile(filepath.Join(instanceDir, "control.token"), []byte(token+"\n"), 0o640); err != nil {
		t.Fatalf("write token: %v", err)
	}
	manager := &operations{points: []recovery.Point{
		{SchemaVersion: 1, ID: "20260828T123456Z-abcdef12", InstanceID: "primary", Reason: "manual-backup"},
		{SchemaVersion: 1, ID: "20260827T123456Z-abcdef12", InstanceID: "primary", Reason: "update-to-2.4.0"},
		{SchemaVersion: 1, ID: "20260826T123456Z-abcdef12", InstanceID: "primary", Reason: "update-to-2.3.0"},
	}}
	authorizer := &mutationAuthorizer{}
	handler := agent.Server{StateDir: stateDir, Operations: manager, Authorization: authorizer}.Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/rollbacks", strings.NewReader(`{"recovery_point_id":"20260826T123456Z-abcdef12"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GEOFlow-Updater-Authorization", "123456")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || len(authorizer.calls) != 0 || len(manager.rollbacks) != 0 {
		t.Fatalf("status = %d, authorization calls = %#v, rollback calls = %#v", response.Code, authorizer.calls, manager.rollbacks)
	}

	accepted := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/rollbacks", strings.NewReader(`{"recovery_point_id":"20260827T123456Z-abcdef12"}`))
	accepted.Header.Set("Authorization", "Bearer "+token)
	accepted.Header.Set("X-GEOFlow-Updater-Authorization", "654321")
	acceptedResponse := httptest.NewRecorder()

	handler.ServeHTTP(acceptedResponse, accepted)

	if acceptedResponse.Code != http.StatusAccepted || len(authorizer.calls) != 1 || len(manager.rollbacks) != 1 {
		t.Fatalf("accepted status = %d, authorization calls = %#v, rollback calls = %#v", acceptedResponse.Code, authorizer.calls, manager.rollbacks)
	}
}

func TestMutationAuthorizationRateLimitUsesTooManyRequests(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	if err := os.MkdirAll(instanceDir, 0o750); err != nil {
		t.Fatalf("mkdir instance: %v", err)
	}
	token := strings.Repeat("d", 43)
	if err := os.WriteFile(filepath.Join(instanceDir, "control.token"), []byte(token+"\n"), 0o640); err != nil {
		t.Fatalf("write token: %v", err)
	}
	manager := &operations{}
	handler := agent.Server{
		StateDir:      stateDir,
		Operations:    manager,
		Authorization: &mutationAuthorizer{err: authorization.ErrRateLimited},
	}.Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/updates", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GEOFlow-Updater-Authorization", "123456")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests || manager.updates != 0 {
		t.Fatalf("status = %d, operation calls = %d", response.Code, manager.updates)
	}
}

func TestRecoveryPointResponseOmitsHostPathsAndArtifactDetails(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	if err := os.MkdirAll(instanceDir, 0o750); err != nil {
		t.Fatalf("mkdir instance: %v", err)
	}
	token := strings.Repeat("c", 43)
	if err := os.WriteFile(filepath.Join(instanceDir, "control.token"), []byte(token+"\n"), 0o640); err != nil {
		t.Fatalf("write token: %v", err)
	}
	handler := agent.Server{StateDir: stateDir, Operations: &operations{}, Authorization: &mutationAuthorizer{}}.Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/instances/primary/backups", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"root"`) || strings.Contains(response.Body.String(), `"files"`) {
		t.Fatalf("response exposed recovery internals: %s", response.Body.String())
	}
}
