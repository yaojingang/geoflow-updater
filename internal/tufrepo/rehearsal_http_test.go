package tufrepo_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRehearsalRecognizesRejectedRollbackAcrossAdminLayouts(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "phase-c-rehearsal.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	var functions strings.Builder
	for _, name := range []string{"extract_csrf_token", "website_get", "website_post", "website_expect_validation_error"} {
		start := strings.Index(source, "\n"+name+"() {\n")
		if start < 0 {
			t.Fatalf("missing rehearsal HTTP function %s", name)
		}
		body := source[start+1:]
		end := strings.Index(body, "\n}\n")
		if end < 0 {
			t.Fatalf("incomplete rehearsal HTTP function %s", name)
		}
		functions.WriteString(body[:end+3])
	}
	start := strings.Index(source, "current_check=website-rollback\n")
	if start < 0 {
		t.Fatal("missing website rollback scenario")
	}
	scenario := source[start:]
	start = strings.Index(scenario, "website_expect_validation_error ")
	end := strings.Index(scenario, "\ndocker exec ")
	if start < 0 || end <= start {
		t.Fatal("missing rejected-rollback HTTP invocation")
	}
	invocation := scenario[start:end]
	const failureMessage = "操作提交失败，请稍后重试并查看服务端日志。"
	const legacyError = `<div class="admin-flash-alert mb-4 bg-red-100"><div>` + failureMessage + `</div></div>`
	for _, tt := range []struct {
		name             string
		html             string
		postStatus       int
		operationChanged bool
		logScanFails     bool
		wantFailure      bool
	}{
		{name: "legacy", html: legacyError},
		{name: "v3", html: `<div class="gf-flash gf-flash--danger admin-flash-alert" role="alert" data-admin-errors><div>` + failureMessage + `</div></div>`},
		{name: "empty-page", wantFailure: true},
		{name: "missing-error-container", html: `<p>` + failureMessage + `</p>`, wantFailure: true},
		{name: "wrong-error-message", html: `<div class="admin-flash-alert">unrelated error</div>`, wantFailure: true},
		{name: "post-rejected-before-controller", html: legacyError, postStatus: http.StatusForbidden, wantFailure: true},
		{name: "operation-unexpectedly-started", html: legacyError, operationChanged: true, wantFailure: true},
		{name: "sensitive-log-gate", html: legacyError, logScanFails: true, wantFailure: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var gets, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/system-updates/updater/rollback" {
					posts.Add(1)
					if r.ParseForm() != nil || r.Form.Get("_token") != "fixture-csrf" ||
						r.Form.Get("recovery_point_id") != "manual-fixture" ||
						r.Form.Get("updater_authorization_code") != "123456" ||
						r.Form.Get("current_admin_password") != "fixture-password" {
						t.Error("rehearsal did not relay the rejected rollback form")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.Header().Set("Location", "/system-updates")
					status := tt.postStatus
					if status == 0 {
						status = http.StatusFound
					}
					w.WriteHeader(status)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/system-updates" {
					t.Error("unexpected rehearsal HTTP request")
					w.WriteHeader(http.StatusNotFound)
					return
				}
				gets.Add(1)
				fmt.Fprint(w, `<input name="_token" value="fixture-csrf">`)
				if posts.Load() > 0 {
					fmt.Fprint(w, tt.html)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-euc", `
website_base_url=$1
fixture_root=$2
operation_changed=$3
log_scan_fails=$4
website_cookie_jar=$fixture_root/cookies
mktemp() { command mktemp "$fixture_root/${1##*/}"; }
admin_password=fixture-password
rollback_code=123456
manual_backup=manual-fixture
mask_value() { :; }
current_operation_id() {
    if test -f "$fixture_root/operation-read" && test "$operation_changed" = true; then
        printf 'unexpected-new-operation\n'
    else
        touch "$fixture_root/operation-read"
        printf 'previous-operation\n'
    fi
}
scan_runtime_logs() { test "$log_scan_fails" = false; }
`+functions.String()+"\n"+invocation+"\nprintf 'rejection-verified\\n'", "rehearsal-http-test", server.URL, t.TempDir(), fmt.Sprint(tt.operationChanged), fmt.Sprint(tt.logScanFails))
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("rehearsal HTTP check timed out")
			}
			if tt.wantFailure {
				exit, failed := err.(*exec.ExitError)
				if !failed || exit.ExitCode() != 1 || strings.Contains(string(output), "rejection-verified") {
					t.Fatalf("invalid rejection evidence was accepted or failed before its assertion: %v; %s", err, output)
				}
			} else if err != nil || string(output) != "rejection-verified\n" {
				t.Fatalf("rejected rollback was not recognized: %v; %s", err, output)
			}
			wantGets := int32(2)
			if tt.postStatus != 0 || tt.logScanFails {
				wantGets = 1
			}
			if gets.Load() != wantGets || posts.Load() != 1 {
				t.Fatalf("HTTP sequence: got %d GET / %d POST, want %d / 1", gets.Load(), posts.Load(), wantGets)
			}
		})
	}
}
