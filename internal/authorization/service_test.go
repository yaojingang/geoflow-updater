package authorization_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/authorization"
)

func TestProvisionRequiresEnrollmentAndCreatesThreeStableRootOnlyFactors(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	service := authorization.Service{StateDir: stateDir}
	if _, err := service.Provision("primary"); err == nil {
		t.Fatal("Provision() before enrollment succeeded")
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "instances", "primary")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-enrollment provisioning created state: %v", err)
	}
	writeEnrollment(t, stateDir)

	first, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	second, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("second Provision() error = %v", err)
	}
	if len(first.Factors) != 3 || len(second.Factors) != 3 {
		t.Fatalf("factor counts = %d and %d, want 3", len(first.Factors), len(second.Factors))
	}
	seenURIs := map[string]struct{}{}
	for index, factor := range first.Factors {
		if factor != second.Factors[index] {
			t.Fatalf("factor %d is unstable: %#v then %#v", index, factor, second.Factors[index])
		}
		parsed, err := url.Parse(factor.URI)
		if err != nil {
			t.Fatalf("parse provisioning URI: %v", err)
		}
		if parsed.Scheme != "otpauth" || parsed.Host != "totp" || parsed.Query().Get("issuer") != "GEOFlow Updater" || parsed.Query().Get("secret") == "" {
			t.Fatalf("provisioning URI = %q", factor.URI)
		}
		seenURIs[factor.URI] = struct{}{}
	}
	if len(seenURIs) != 3 {
		t.Fatalf("operation factors are not distinct: %#v", first.Factors)
	}
	secretPath := filepath.Join(stateDir, "instances", "primary", "mutation.secret")
	info, err := os.Lstat(secretPath)
	if err != nil {
		t.Fatalf("stat secret: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode = %v, want regular 0600", info.Mode())
	}
}

func TestAuthorizeBindsCodesToScopesAndConsumesOnlyAfterAcceptedMutation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC)
	stateDir := t.TempDir()
	writeEnrollment(t, stateDir)
	service := authorization.Service{StateDir: stateDir, Now: func() time.Time { return now }}
	provisioned, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	updateCode := codeFor(t, provisioned, authorization.ScopeUpdate, now)
	backupCode := codeFor(t, provisioned, authorization.ScopeBackup, now)

	if err := service.Authorize("primary", authorization.ScopeBackup, updateCode, func() error { return nil }); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("cross-scope authorization error = %v, want ErrInvalid", err)
	}
	rejected := errors.New("operation active")
	if err := service.Authorize("primary", authorization.ScopeUpdate, updateCode, func() error { return rejected }); !errors.Is(err, rejected) {
		t.Fatalf("rejected mutation error = %v, want callback error", err)
	}
	accepted := 0
	if err := service.Authorize("primary", authorization.ScopeUpdate, updateCode, func() error { accepted++; return nil }); err != nil {
		t.Fatalf("Authorize(update) error = %v", err)
	}
	if accepted != 1 {
		t.Fatalf("accepted callbacks = %d, want 1", accepted)
	}
	if err := service.Authorize("primary", authorization.ScopeUpdate, updateCode, func() error { accepted++; return nil }); !errors.Is(err, authorization.ErrReplay) {
		t.Fatalf("replayed update error = %v, want ErrReplay", err)
	}
	if err := service.Authorize("primary", authorization.ScopeBackup, backupCode, func() error { return nil }); err != nil {
		t.Fatalf("Authorize(backup) error = %v", err)
	}
}

func TestAuthorizeAllowsOnlyOneConcurrentUse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC)
	stateDir := t.TempDir()
	writeEnrollment(t, stateDir)
	service := authorization.Service{StateDir: stateDir, Now: func() time.Time { return now }}
	provisioned, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	code := codeFor(t, provisioned, authorization.ScopeUpdate, now)

	const consumers = 8
	results := make(chan error, consumers)
	start := make(chan struct{})
	var callbacks atomic.Int32
	var group sync.WaitGroup
	for range consumers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- service.Authorize("primary", authorization.ScopeUpdate, code, func() error {
				callbacks.Add(1)
				return nil
			})
		}()
	}
	close(start)
	group.Wait()
	close(results)

	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, authorization.ErrReplay) {
			t.Fatalf("concurrent Authorize() error = %v", err)
		}
	}
	if accepted != 1 || callbacks.Load() != 1 {
		t.Fatalf("accepted = %d, callbacks = %d, want 1 each", accepted, callbacks.Load())
	}
}

func TestAuthorizeUsesPersistentCrossWindowExponentialLockout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC)
	stateDir := t.TempDir()
	writeEnrollment(t, stateDir)
	service := authorization.Service{StateDir: stateDir, Now: func() time.Time { return now }}
	provisioned, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	valid := codeFor(t, provisioned, authorization.ScopeUpdate, now)
	invalid := differentCode(valid)
	for attempt := 0; attempt < 4; attempt++ {
		if err := service.Authorize("primary", authorization.ScopeUpdate, invalid, func() error { return nil }); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("invalid attempt %d error = %v, want ErrInvalid", attempt+1, err)
		}
	}
	if err := service.Authorize("primary", authorization.ScopeUpdate, invalid, func() error { return nil }); !errors.Is(err, authorization.ErrRateLimited) {
		t.Fatalf("fifth invalid attempt error = %v, want ErrRateLimited", err)
	}
	now = now.Add(14 * time.Minute)
	if err := service.Authorize("primary", authorization.ScopeUpdate, codeFor(t, provisioned, authorization.ScopeUpdate, now), func() error { return nil }); !errors.Is(err, authorization.ErrRateLimited) {
		t.Fatalf("authorization during lockout error = %v, want ErrRateLimited", err)
	}
	now = now.Add(time.Minute)
	if err := service.Authorize("primary", authorization.ScopeUpdate, codeFor(t, provisioned, authorization.ScopeUpdate, now), func() error { return nil }); err != nil {
		t.Fatalf("valid authorization after lockout error = %v", err)
	}
	if err := service.Authorize("primary", authorization.ScopeUpdate, invalid, func() error { return nil }); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("post-success invalid attempt error = %v, want reset ErrInvalid", err)
	}
	attemptsInfo, err := os.Lstat(filepath.Join(stateDir, "instances", "primary", "mutation.update.attempts"))
	if err != nil {
		t.Fatalf("stat attempt state: %v", err)
	}
	if attemptsInfo.Mode().Perm() != 0o600 {
		t.Fatalf("attempt state mode = %v, want 0600", attemptsInfo.Mode())
	}
}

func TestAuthorizeKeepsFailureStateIsolatedBetweenScopes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC)
	stateDir := t.TempDir()
	writeEnrollment(t, stateDir)
	service := authorization.Service{StateDir: stateDir, Now: func() time.Time { return now }}
	provisioned, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	validRollback := codeFor(t, provisioned, authorization.ScopeRollback, now)
	invalidRollback := differentCode(validRollback)
	for attempt := 0; attempt < 4; attempt++ {
		if err := service.Authorize("primary", authorization.ScopeRollback, invalidRollback, func() error { return nil }); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("invalid rollback attempt %d error = %v, want ErrInvalid", attempt+1, err)
		}
	}
	validBackup := codeFor(t, provisioned, authorization.ScopeBackup, now)
	if err := service.Authorize("primary", authorization.ScopeBackup, validBackup, func() error { return nil }); err != nil {
		t.Fatalf("Authorize(backup) error = %v", err)
	}
	if err := service.Authorize("primary", authorization.ScopeRollback, invalidRollback, func() error { return nil }); !errors.Is(err, authorization.ErrRateLimited) {
		t.Fatalf("fifth rollback attempt after accepted backup error = %v, want ErrRateLimited", err)
	}
}

func TestAuthorizeRateLimitsAttemptsDistributedAcrossScopes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC)
	stateDir := t.TempDir()
	writeEnrollment(t, stateDir)
	service := authorization.Service{StateDir: stateDir, Now: func() time.Time { return now }}
	provisioned, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	sequence := []authorization.Scope{
		authorization.ScopeUpdate,
		authorization.ScopeBackup,
		authorization.ScopeRollback,
		authorization.ScopeUpdate,
		authorization.ScopeBackup,
	}
	for index, scope := range sequence {
		invalid := differentCode(codeFor(t, provisioned, scope, now))
		err := service.Authorize("primary", scope, invalid, func() error { return nil })
		if index < 4 && !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("distributed attempt %d error = %v, want ErrInvalid", index+1, err)
		}
		if index == 4 && !errors.Is(err, authorization.ErrRateLimited) {
			t.Fatalf("distributed attempt %d error = %v, want ErrRateLimited", index+1, err)
		}
	}
}

func TestAuthorizeKeepsFailureStateWhenTheMutationIsRejected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC)
	stateDir := t.TempDir()
	writeEnrollment(t, stateDir)
	service := authorization.Service{StateDir: stateDir, Now: func() time.Time { return now }}
	provisioned, err := service.Provision("primary")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	valid := codeFor(t, provisioned, authorization.ScopeUpdate, now)
	invalid := differentCode(valid)
	for attempt := 0; attempt < 4; attempt++ {
		if err := service.Authorize("primary", authorization.ScopeUpdate, invalid, func() error { return nil }); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("invalid attempt %d error = %v, want ErrInvalid", attempt+1, err)
		}
	}
	rejected := errors.New("operation active")
	if err := service.Authorize("primary", authorization.ScopeUpdate, valid, func() error { return rejected }); !errors.Is(err, rejected) {
		t.Fatalf("rejected mutation error = %v, want callback error", err)
	}
	if err := service.Authorize("primary", authorization.ScopeUpdate, invalid, func() error { return nil }); !errors.Is(err, authorization.ErrRateLimited) {
		t.Fatalf("fifth attempt after rejected mutation error = %v, want ErrRateLimited", err)
	}
}

func writeEnrollment(t *testing.T, stateDir string) {
	t.Helper()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	if err := os.MkdirAll(instanceDir, 0o750); err != nil {
		t.Fatalf("mkdir instance: %v", err)
	}
	contents := "schema_version: 1\n" +
		"id: primary\n" +
		"root: /opt/geoflow\n" +
		"compose_file: " + filepath.Join(instanceDir, "docker-compose.managed.yml") + "\n" +
		"environment_file: " + filepath.Join(instanceDir, "release.env") + "\n" +
		"control_token_file: " + filepath.Join(instanceDir, "control.token") + "\n" +
		"release_sequence: 1\n" +
		"version: 1.0.0\n" +
		"postgres_major: 18\n" +
		"postgres_data_dir: /opt/geoflow/docker-data/prod/postgres\n" +
		"postgres_container_data_dir: /var/lib/postgresql\n" +
		"redis_major: 8\n" +
		"enrolled_at: 2026-08-27T12:00:00Z\n"
	if err := os.WriteFile(filepath.Join(instanceDir, "instance.yml"), []byte(contents), 0o640); err != nil {
		t.Fatalf("write enrollment: %v", err)
	}
}

func codeFor(t *testing.T, provisioning authorization.Provisioning, scope authorization.Scope, now time.Time) string {
	t.Helper()
	for _, factor := range provisioning.Factors {
		if factor.Scope != scope {
			continue
		}
		parsed, err := url.Parse(factor.URI)
		if err != nil {
			t.Fatalf("parse factor URI: %v", err)
		}

		return totpCode(t, parsed.Query().Get("secret"), now.Unix()/30)
	}
	t.Fatalf("scope %q factor not found", scope)

	return ""
}

func differentCode(valid string) string {
	if valid == "000000" {
		return "000001"
	}

	return "000000"
}

func totpCode(t *testing.T, encodedSecret string, counter int64) string {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(encodedSecret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	message := make([]byte, 8)
	binary.BigEndian.PutUint64(message, uint64(counter))
	digest := hmac.New(sha1.New, secret)
	_, _ = digest.Write(message)
	sum := digest.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	return leftPad(strconv.FormatUint(uint64(value%1_000_000), 10), 6)
}

func leftPad(value string, width int) string {
	for len(value) < width {
		value = "0" + value
	}

	return value
}
