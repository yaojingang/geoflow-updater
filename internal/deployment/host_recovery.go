package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
)

var recoveryDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

//go:embed recovery-redis.php
var recoveryRedisScript []byte

type recoveryReport struct {
	SchemaVersion          int    `json:"schema_version"`
	Status                 string `json:"status"`
	AdminDigest            string `json:"admin_digest,omitempty"`
	TransactionID          string `json:"transaction_id,omitempty"`
	Epoch                  string `json:"epoch,omitempty"`
	CredentialsInvalidated bool   `json:"credentials_invalidated,omitempty"`
	AdministratorsVerified bool   `json:"administrators_verified,omitempty"`
	ThemeRevisions         *int   `json:"theme_revisions"`
	BackgroundStatus       string `json:"background_status,omitempty"`
	QuarantineCount        *int   `json:"quarantine_count"`
	QuarantineSHA256       string `json:"quarantine_sha256,omitempty"`
}

func (service *Service) prepareRestore(ctx context.Context, config instance.Config, request recovery.RestoreRequest) (recovery.RestoreRequest, error) {
	if !safeOperationID(request.TransactionID) {
		return request, errors.New("recovery requires a stable operation identity")
	}
	control := service.recoveryControl()
	state, err := control.Initialize(config.ID)
	if err != nil {
		return request, err
	}
	if state.State.TransactionID != nil && *state.State.TransactionID == request.TransactionID {
		if state.PointID != request.PointID {
			return request, errors.New("recovery point changed within transaction")
		}
		request.AdminDigest = state.AdminDigest
		return request, nil
	}
	if checkpoint, readErr := control.Checkpoint(config.ID, request.PointID, request.TransactionID); readErr == nil {
		request.AdminDigest = checkpoint.AdminDigest
		return request, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return request, readErr
	}
	report, err := service.recoveryPhase(ctx, config, "inspect", request.TransactionID, "")
	if err != nil {
		return request, err
	}
	if !recoveryDigest.MatchString(report.AdminDigest) {
		return request, errors.New("Core did not provide a frozen administrator digest")
	}
	request.AdminDigest = report.AdminDigest
	return request, nil
}

func (service *Service) recoveryPhase(ctx context.Context, config instance.Config, phase, transaction, digest string) (recoveryReport, error) {
	var report recoveryReport
	if phase != "inspect" && phase != "prepare" && phase != "verify" {
		return report, errors.New("unsupported fixed recovery phase")
	}
	if !safeOperationID(transaction) {
		return report, errors.New("invalid recovery operation identity")
	}
	legacy, err := legacyAdapterRequired(config)
	if err != nil {
		return report, err
	}
	args := []string{"run", "--rm", "--no-deps", "--user", "33:33", "--entrypoint", "php", "-e", "AUTO_MIGRATE=false", "-e", "AUTO_INSTALL_ONCE=false", "-e", "AUTO_OPTIMIZE=false", "init"}
	if legacy {
		args = append(args, legacyAdapterContainerDir+"/legacy-recovery.php")
	} else {
		args = append(args, "artisan", "geoflow:recovery")
	}
	args = append(args, "--phase="+phase, "--transaction="+transaction, "--json", "--no-interaction")
	if phase == "prepare" {
		if !recoveryDigest.MatchString(digest) {
			return report, errors.New("invalid frozen administrator digest")
		}
		args = append(args, "--expected-admin-digest="+digest)
	}
	var out boundedOutput
	arguments, err := service.runtimeArguments(config)
	if err != nil {
		return report, err
	}
	if err = service.runner().Run(ctx, nil, &out, "docker", append(arguments, args...)...); err != nil {
		return report, fmt.Errorf("isolated Core recovery %s failed: %w", phase, err)
	}
	if out.exceeded {
		return report, errors.New("Core recovery response exceeded limit")
	}
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		return report, errors.New("Core recovery returned invalid JSON; compatible recovery adapter is required")
	}
	if report.SchemaVersion != 1 || report.Status != "pass" || report.ThemeRevisions == nil || *report.ThemeRevisions < 0 {
		return report, errors.New("Core recovery evidence was incomplete")
	}
	return report, nil
}

func (service *Service) resumeRecovered(ctx context.Context, config instance.Config, a recoverycontrol.Authority) error {
	control := service.recoveryControl()
	if a.State.TransactionID == nil || !a.DataRestored || (a.State.Phase != "validating" && a.State.Phase != "http_ready") {
		return errors.New("data recovery is incomplete; services remain stopped")
	}
	if err := service.startServices(ctx, infrastructureConfig(config), "postgres", "redis"); err != nil {
		return err
	}
	transaction := *a.State.TransactionID
	if a.State.Phase == "validating" {
		var verified recoveryReport
		for _, phase := range []string{"prepare", "verify"} {
			report, err := service.recoveryPhase(ctx, config, phase, transaction, a.AdminDigest)
			if err != nil {
				return err
			}
			if report.TransactionID != transaction || report.Epoch != a.State.Epoch || !report.CredentialsInvalidated || !report.AdministratorsVerified || report.BackgroundStatus != "held" || report.QuarantineCount == nil || *report.QuarantineCount < 0 || !recoveryDigest.MatchString(report.QuarantineSHA256) {
				return errors.New("Core recovery proof does not authorize HTTP startup")
			}
			if phase == "verify" {
				before, _ := json.Marshal(verified)
				after, _ := json.Marshal(report)
				if !bytes.Equal(before, after) {
					return errors.New("Core preparation changed during verification")
				}
			}
			verified = report
		}
		encoded, _ := json.Marshal(verified)
		proof := sha256.Sum256(encoded)
		if _, err := control.RecordPreparation(config.ID, transaction, hex.EncodeToString(proof[:])); err != nil {
			return err
		}
		if err := service.quarantineRedis(ctx, config, a); err != nil {
			return err
		}
		var err error
		a, err = control.OpenHTTP(config.ID, transaction)
		if err != nil {
			return err
		}
	}
	if a.PreparationSHA256 == "" || a.RedisManifestSHA256 == "" {
		return errors.New("missing durable recovery validation proof")
	}
	if err := service.command(ctx, config, "run", "--rm", "--no-deps", "--user", "33:33", "--entrypoint", "php", "init", "artisan", "up"); err != nil {
		return err
	}
	if err := service.startServices(ctx, config, "app", "web"); err != nil {
		return err
	}
	if config.Layout == LayoutBlueGreen {
		if _, err := service.switchIngress(ctx, config, config.ReleaseSequence, true); err != nil {
			return err
		}
	}
	return service.checkHTTP(ctx, config)
}

func (service *Service) quarantineRedis(ctx context.Context, config instance.Config, a recoverycontrol.Authority) error {
	control := service.recoveryControl()
	dir := filepath.Join(control.StateDir, "recovery-control", config.ID, "scripts")
	if err := safeDirectory(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "recovery-redis.php")
	if err := replaceContents(path, recoveryRedisScript, 0444); err != nil {
		return err
	}
	arguments, err := service.runtimeArguments(config)
	if err != nil {
		return err
	}
	args := []string{"run", "--rm", "--no-deps", "--user", "33:33", "--entrypoint", "php", "-v", path + ":/run/geoflow-recovery-redis.php:ro", "init", "/run/geoflow-recovery-redis.php", a.State.Epoch}
	var out boundedOutput
	if err = service.runner().Run(ctx, nil, &out, "docker", append(arguments, args...)...); err != nil {
		return fmt.Errorf("Redis quarantine failed: %w", err)
	}
	if out.exceeded {
		return errors.New("Redis quarantine manifest exceeded limit")
	}
	var report struct {
		SchemaVersion int               `json:"schema_version"`
		Status        string            `json:"status"`
		Epoch         string            `json:"epoch"`
		Entries       []json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(out.Bytes(), &report) != nil || report.SchemaVersion != 1 || report.Status != "pass" || report.Epoch != a.State.Epoch || report.Entries == nil {
		return errors.New("invalid Redis quarantine evidence")
	}
	canonical, err := json.Marshal(report)
	if err != nil {
		return err
	}
	directory := filepath.Join(control.StateDir, "recovery-control", config.ID, "quarantine", a.State.Epoch)
	if err = safeDirectory(directory, 0700); err != nil {
		return err
	}
	manifest := filepath.Join(directory, "queues.json")
	if previous, readErr := os.ReadFile(manifest); readErr == nil && !bytes.Equal(previous, canonical) {
		return errors.New("Redis quarantine evidence changed")
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err = replaceContents(manifest, canonical, 0600); err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	_, err = control.RecordRedisManifest(config.ID, *a.State.TransactionID, hex.EncodeToString(sum[:]))
	return err
}

func (service *Service) requireRecoveryReady(id string) error {
	control := service.recoveryControl()
	a, err := control.Read(id)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(filepath.Join(control.StateDir, "recovery-control", id, "initialized")); errors.Is(e, os.ErrNotExist) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if a.State.Phase != "ready" {
		return errors.New("recovery reconciliation is required before application changes or background work")
	}
	return nil
}

var _ io.Writer = (*boundedOutput)(nil)

func (service *Service) FreezeRecoveryCheckpoint(ctx context.Context, id string, request recovery.RestoreRequest) error {
	config, err := service.loadConfig(id)
	if err != nil {
		return err
	}
	request, err = service.prepareRestore(ctx, config, request)
	if err != nil {
		return err
	}
	return service.recoveryControl().CaptureCheckpoint(id, request.PointID, request.TransactionID, request.AdminDigest)
}
