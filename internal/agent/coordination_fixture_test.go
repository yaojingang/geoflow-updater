package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/operation"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type fixtureStatus struct{}

func (fixtureStatus) Run(context.Context, string) doctor.Report {
	return doctor.Report{SchemaVersion: 1, Status: doctor.StatusPass, Instance: &instance.Config{ID: "primary", Version: "3.1.0", ReleaseSequence: 12}, Checks: []doctor.Check{{ID: "retired-update-worker", Status: doctor.StatusPass, Message: "isolated fixture has no retired worker"}}}
}

type fixtureDeployment struct{ dir string }

func (d fixtureDeployment) ActionSnapshot(_ context.Context, _, action, _ string) (coordination.Snapshot, error) {
	if action != "backup" {
		return coordination.Snapshot{}, errors.New("fixture only supports backup")
	}
	return coordination.Snapshot{BaselineSHA256: strings.Repeat("a", 64), MaintenanceRequired: true, Continuation: "host_only", Details: json.RawMessage(`{"source":{"version":"3.1.0","sequence":12},"scope":["database","storage","configuration","deployment","redis"]}`)}, nil
}
func (fixtureDeployment) Resolve(context.Context, string) (managed.Release, error) {
	return managed.Release{}, errors.New("fixture has no update")
}
func (fixtureDeployment) Preflight(context.Context, string, managed.Release) error { return nil }
func (fixtureDeployment) Pull(context.Context, string, managed.Release) error      { return nil }
func (fixtureDeployment) Quiesce(context.Context, string) error                    { return nil }
func (fixtureDeployment) QuiesceForRecovery(context.Context, string, string) error {
	return errors.New("fixture has no restore")
}
func (d fixtureDeployment) CreateRecoveryPoint(context.Context, string, string) (string, error) {
	path := filepath.Join(d.dir, "executions")
	previous, _ := os.ReadFile(path)
	count, _ := strconv.Atoi(strings.TrimSpace(string(previous)))
	if err := os.WriteFile(path, []byte(strconv.Itoa(count+1)), 0600); err != nil {
		return "", err
	}
	return "20260916T120000Z-00000001", nil
}
func (fixtureDeployment) Migrate(context.Context, string, managed.Release) error  { return nil }
func (fixtureDeployment) Activate(context.Context, string, managed.Release) error { return nil }
func (fixtureDeployment) Rollback(context.Context, string, recovery.RestoreRequest) error {
	return errors.New("fixture has no restore")
}
func (fixtureDeployment) Resume(context.Context, string) error       { return nil }
func (fixtureDeployment) Verify(context.Context, string) error       { return nil }
func (fixtureDeployment) ValidateRecoveryPoint(string, string) error { return nil }
func (fixtureDeployment) ListRecoveryPoints(string) ([]recovery.Point, error) {
	return []recovery.Point{}, nil
}

type fixtureDescriptor struct {
	Socket           string `json:"socket"`
	StateDir         string `json:"state_dir"`
	ControlTokenFile string `json:"control_token_file"`
	Epoch            string `json:"epoch"`
	OTP              string `json:"otp"`
	OTPCounter       int64  `json:"otp_counter"`
	HostID           string `json:"host_id"`
}

func newCoordinationFixture(t *testing.T, dir string) (Server, *operation.Manager, func() fixtureDescriptor) {
	t.Helper()
	state := filepath.Join(dir, "state")
	instance := filepath.Join(state, "instances", "primary")
	if err := os.MkdirAll(instance, 0750); err != nil {
		t.Fatal(err)
	}
	config := "schema_version: 1\nid: primary\nroot: " + dir + "\ncompose_file: " + filepath.Join(instance, "compose.yml") + "\nenvironment_file: " + filepath.Join(instance, "release.env") + "\ncontrol_token_file: " + filepath.Join(instance, "control.token") + "\nrelease_sequence: 12\nversion: 3.1.0\nenrolled_at: 2026-09-16T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(instance, "instance.yml"), []byte(config), 0640); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(instance, "control.token")
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("a", 43)), 0640); err != nil {
		t.Fatal(err)
	}
	authority, err := (recoverycontrol.Store{StateDir: state}).Initialize("primary")
	if err != nil {
		t.Fatal(err)
	}
	auth := authorization.Service{StateDir: state}
	provisioning, err := auth.Provision("primary")
	if err != nil {
		t.Fatal(err)
	}
	secret := ""
	for _, factor := range provisioning.Factors {
		if factor.Scope == authorization.ScopeBackup {
			uri, _ := url.Parse(factor.URI)
			secret = uri.Query().Get("secret")
		}
	}
	deployment := fixtureDeployment{dir: dir}
	manager := &operation.Manager{StateDir: state, Deployment: deployment, Engine: update.Engine{Deployment: deployment}}
	descriptor := func() fixtureDescriptor {
		counter := time.Now().Unix() / 30
		key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
		message := make([]byte, 8)
		binary.BigEndian.PutUint64(message, uint64(counter))
		h := hmac.New(sha1.New, key)
		_, _ = h.Write(message)
		sum := h.Sum(nil)
		offset := sum[len(sum)-1] & 15
		value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
		return fixtureDescriptor{Socket: filepath.Join(dir, "agent.sock"), StateDir: state, ControlTokenFile: tokenPath, Epoch: authority.State.Epoch, OTP: fmt.Sprintf("%06d", value%1000000), OTPCounter: counter, HostID: authority.State.HostID}
	}
	return Server{StateDir: state, Version: "fixture", Status: fixtureStatus{}, Operations: manager, Authorization: auth}, manager, descriptor
}

func TestCoordinatedHTTPAdmissionAndReplay(t *testing.T) {
	server, manager, describe := newCoordinationFixture(t, t.TempDir())
	handler := server.Handler()
	descriptor := describe()
	send := func(method, path string, payload any, otp string) *httptest.ResponseRecorder {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = strings.NewReader(string(b))
		}
		r := httptest.NewRequest(method, "/v2/instances/primary/"+path, body)
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 43))
		r.Header.Set("X-GEOFlow-Updater-Authorization", otp)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	actor := coordination.Actor{ManagementInstanceID: "12345678-1234-4234-8234-123456789012", AdminID: 1, IdentitySHA256: strings.Repeat("b", 64)}
	planned := send("POST", "plans", coordination.PlanRequest{Action: "backup", ExpectedEpoch: descriptor.Epoch, Actor: actor}, "")
	if planned.Code != 201 {
		t.Fatal(planned.Code, planned.Body)
	}
	var p coordination.Plan
	if err := json.Unmarshal(planned.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot != nil {
		t.Fatal("private snapshot exposed")
	}
	request := coordination.SubmitRequest{Action: "backup", PlanID: p.PlanID, PlanSHA256: p.PlanSHA256, ExpectedEpoch: p.ExpectedEpoch, AllowMaintenance: true, ConfirmHostAccess: true, ClientRequestID: "http-request-0001", Actor: actor, Scope: "updater:backup"}
	accepted := send("POST", "operations", request, descriptor.OTP)
	if accepted.Code != 202 {
		t.Fatal(accepted.Code, accepted.Body)
	}
	manager.Wait(context.Background())
	var first coordination.Receipt
	if err := json.Unmarshal(accepted.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		repeat := send("POST", "operations", request, "")
		if repeat.Code != 202 {
			t.Fatal(repeat.Code, repeat.Body)
		}
		var receipt coordination.Receipt
		_ = json.Unmarshal(repeat.Body.Bytes(), &receipt)
		if first.OperationID != receipt.OperationID {
			t.Fatal("operation identity changed")
		}
	}
	receipt := send("GET", "requests/"+request.ClientRequestID, nil, "")
	if receipt.Code != 200 {
		t.Fatal(receipt.Code, receipt.Body)
	}
	op := send("GET", "operations/"+first.OperationID, nil, "")
	if op.Code != 200 {
		t.Fatal(op.Code, op.Body)
	}
	// A distinct request cannot consume the same factor window through v1.
	legacy := httptest.NewRequest(http.MethodPost, "/v1/instances/primary/backups", nil)
	legacy.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 43))
	legacy.Header.Set("X-GEOFlow-Updater-Authorization", descriptor.OTP)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, legacy)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "replayed") {
		t.Fatal(w.Code, w.Body)
	}
	executions, _ := os.ReadFile(filepath.Join(filepath.Dir(descriptor.Socket), "executions"))
	if string(executions) != "1" {
		t.Fatal("execution count", string(executions))
	}
}

// TestCoordinatedSocketFixture is an explicit, isolated integration harness for
// the Core PHP/CLI client. It is unavailable in the production binary.
func TestCoordinatedSocketFixture(t *testing.T) {
	dir := os.Getenv("GEOFLOW_COORDINATION_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set GEOFLOW_COORDINATION_FIXTURE_DIR to an isolated temporary directory")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil || (!strings.HasPrefix(absolute, "/private/tmp/") && !strings.HasPrefix(absolute, "/tmp/")) {
		t.Fatal("fixture must use a temporary directory")
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("fixture directory must exist")
	}
	server, manager, describe := newCoordinationFixture(t, absolute)
	if err := manager.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, describe().Socket, server.Handler()) }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		descriptor := describe()
		contents, _ := json.Marshal(descriptor)
		if err := os.WriteFile(filepath.Join(absolute, "fixture.json.tmp"), contents, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(absolute, "fixture.json.tmp"), filepath.Join(absolute, "fixture.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(absolute, "stop")); err == nil {
			cancel()
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			manager.Wait(context.Background())
			return
		case <-ticker.C:
		}
	}
}
