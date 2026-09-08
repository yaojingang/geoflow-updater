package agent_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/agent"
	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/operation"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type plannedManager struct {
	operations
	previews, switchBacks, planned int
	options                        update.Options
	err                            error
}

func (f *plannedManager) Preview(context.Context, string) (update.PlanSummary, error) {
	f.previews++
	return update.PlanSummary{SchemaVersion: 1, PlanSHA256: strings.Repeat("a", 64)}, f.err
}
func (f *plannedManager) StartUpdateWithOptions(_ string, o update.Options) (operation.Operation, error) {
	f.planned++
	f.options = o
	if o.ExpectedPlanSHA256 == strings.Repeat("b", 64) {
		return operation.Operation{}, errors.New("plan mismatch")
	}
	return operation.Operation{ID: "planned"}, nil
}
func (f *plannedManager) StartSwitchBack(string) (operation.Operation, error) {
	f.switchBacks++
	return operation.Operation{ID: "switch-back"}, nil
}
func TestPlannedAPIRequiresTokenValidPayloadAndCorrectMutationScope(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "instances", "primary")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 43)
	if err := os.WriteFile(filepath.Join(dir, "control.token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	f := &plannedManager{}
	auth := &mutationAuthorizer{}
	handler := agent.Server{StateDir: state, Operations: f, Authorization: auth}.Handler()
	call := func(method, path, body, code string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/instances/primary/"+path, strings.NewReader(body))
		if authorized {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if code != "" {
			req.Header.Set("X-GEOFlow-Updater-Authorization", code)
		}
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, req)
		return r
	}
	if r := call("GET", "plan", "", "", false); r.Code != 401 || f.previews != 0 {
		t.Fatalf("preview token: %d", r.Code)
	}
	if r := call("GET", "plan", "", "", true); r.Code != 200 || f.previews != 1 || len(auth.calls) != 0 {
		t.Fatalf("preview: %d", r.Code)
	}
	for _, body := range []string{"null", "[]", `{"allow_maintenance":false,"allow_maintenance":true}`, `{"allow_maintenance":"true"}`, `{"allow_maintenance":null}`, `{"expected_plan_sha256":"bad"}`, `{"command":"shell"}`, `{} {}`, `{"allow_maintenance":true`} {
		if r := call("POST", "updates", body, "123456", true); r.Code != 400 || f.planned != 0 || len(auth.calls) != 0 {
			t.Fatalf("invalid %s: %d", body, r.Code)
		}
	}
	if r := call("POST", "updates", `{}`, "", true); r.Code != 403 {
		t.Fatalf("missing OTP: %d", r.Code)
	}
	if r := call("POST", "updates", `{}`, "123456", true); r.Code != 202 || f.options.AllowMaintenance {
		t.Fatalf("default update: %d %#v", r.Code, f.options)
	}
	if r := call("POST", "updates", `{"allow_maintenance":true,"expected_plan_sha256":"`+strings.Repeat("a", 64)+`"}`, "123456", true); r.Code != 202 || !f.options.AllowMaintenance {
		t.Fatalf("confirmed: %d", r.Code)
	}
	if r := call("POST", "updates", `{"expected_plan_sha256":"`+strings.Repeat("b", 64)+`"}`, "123456", true); r.Code != 503 || f.updates != 0 {
		t.Fatalf("changed plan fallback: %d", r.Code)
	}
	if r := call("POST", "switch-backs", "", "123456", true); r.Code != 202 || f.switchBacks != 1 || auth.calls[len(auth.calls)-1].scope != authorization.ScopeUpdate || len(f.rollbacks) != 0 {
		t.Fatalf("switchback scope: %d %#v", r.Code, auth.calls)
	}
	auth.err = authorization.ErrInvalid
	if r := call("POST", "switch-backs", "", "654321", true); r.Code != 403 || f.switchBacks != 1 {
		t.Fatalf("wrong scope token: %d", r.Code)
	}
	f.err = errors.New("tampered plan")
	if r := call(http.MethodGet, "plan", "", "", true); r.Code != 503 || f.updates != 0 {
		t.Fatalf("failed preview: %d", r.Code)
	}
}
