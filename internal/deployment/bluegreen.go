package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"gopkg.in/yaml.v3"
)

type releaseTransaction struct {
	SchemaVersion     int             `json:"schema_version"`
	OperationID       string          `json:"operation_id"`
	Status            update.Status   `json:"status"`
	Stage             string          `json:"stage"`
	Strategy          string          `json:"strategy"`
	Source            instance.Config `json:"source"`
	Candidate         instance.Config `json:"candidate"`
	Target            managed.Release `json:"target"`
	SourceVersion     []byte          `json:"source_version"`
	RecoveryPointID   string          `json:"recovery_point_id,omitempty"`
	TrafficOpened     bool            `json:"traffic_opened"`
	LayoutStarted     bool            `json:"layout_started"`
	WorkersHandedOver bool            `json:"workers_handed_over"`
	OldIngressWorkers []string        `json:"old_ingress_workers,omitempty"`
	PlanSHA256        string          `json:"plan_sha256"`
	Error             string          `json:"error,omitempty"`
}

func (service *Service) transactionPath(id string) string {
	return filepath.Join(service.instanceDirectory(id), "release-transaction.json")
}
func (service *Service) saveTransaction(tx *releaseTransaction) error {
	data, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	return replaceContents(service.transactionPath(tx.Source.ID), data, 0600)
}
func (service *Service) readTransaction(id string) (releaseTransaction, error) {
	var tx releaseTransaction
	if !instanceIDPattern.MatchString(id) {
		return tx, errors.New("invalid instance")
	}
	path := service.transactionPath(id)
	if err := regularFile(path); err != nil {
		return tx, err
	}
	file, err := os.Open(path)
	if err != nil {
		return tx, err
	}
	defer file.Close()
	if err := json.NewDecoder(io.LimitReader(file, 8*1024*1024)).Decode(&tx); err != nil {
		return tx, err
	}
	if tx.SchemaVersion != 1 || tx.Source.ID != id || tx.Candidate.ID != id || !validSlot(tx.Candidate.ActiveSlot) || !safeOperationID(tx.OperationID) {
		return tx, errors.New("invalid release transaction")
	}
	for _, config := range []instance.Config{tx.Source, tx.Candidate} {
		for _, path := range []string{config.ComposeFile, config.EnvironmentFile} {
			if !inside(service.instanceDirectory(id), path) {
				return tx, errors.New("release transaction path escaped managed state")
			}
		}
	}
	if err := tx.Target.Validate(); err != nil {
		return tx, err
	}
	return tx, nil
}
func safeOperationID(id string) bool {
	return len(id) > 0 && len(id) <= 100 && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == "" && id != "." && id != ".."
}
func inside(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && filepath.IsAbs(path) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (service *Service) Preview(ctx context.Context, id string) (update.PlanSummary, error) {
	release, err := service.Resolve(ctx, id)
	if err != nil {
		return update.PlanSummary{}, err
	}
	config, err := service.loadConfig(id)
	if err != nil {
		return update.PlanSummary{}, err
	}
	if len(release.UpgradePlan) == 0 {
		return update.PlanSummary{}, errors.New("signed release has no upgrade plan; legacy maintenance update is required")
	}
	if err := service.Preflight(ctx, id, release); err != nil {
		return update.PlanSummary{}, err
	}
	directory, err := os.MkdirTemp(service.instanceDirectory(id), ".preview-")
	if err != nil {
		return update.PlanSummary{}, err
	}
	defer os.RemoveAll(directory)
	slot := ""
	if config.Layout == LayoutBlueGreen {
		slot = otherSlot(config.ActiveSlot)
	}
	candidate, err := service.prepareRelease(ctx, config, release, directory, slot)
	if err != nil {
		return update.PlanSummary{}, err
	}
	if err := service.command(ctx, candidate, "pull", "init"); err != nil {
		return update.PlanSummary{}, err
	}
	report, err := service.applicationPhase(ctx, candidate, release, config.ReleaseSequence, "preview", "inspect", false)
	if err != nil {
		return update.PlanSummary{}, err
	}
	return service.summarize(config, release, report)
}

func (service *Service) summarize(config instance.Config, release managed.Release, report applicationReport) (update.PlanSummary, error) {
	plan, err := release.Plan()
	if err != nil {
		return update.PlanSummary{}, err
	}
	if !plan.AllowsSource(config.ReleaseSequence) {
		return update.PlanSummary{}, errors.New("installed release is outside the signed upgrade source range")
	}
	strategy := plan.Strategy
	if config.Layout != LayoutBlueGreen {
		strategy = managed.StrategyMaintenance
	} else if service.verifyInfrastructurePins(config, release) != nil {
		strategy = managed.StrategyMaintenance
	}
	if strategy == managed.StrategyOnline && !report.EligibleOnline {
		return update.PlanSummary{}, errors.New("pending migrations or application contracts require maintenance")
	}
	if report.PendingMigrations == nil {
		report.PendingMigrations = []managed.UpgradeMigration{}
	}
	summary := update.PlanSummary{SchemaVersion: 1, SourceSequence: config.ReleaseSequence, TargetSequence: release.Sequence, TargetVersion: release.Version, Strategy: strategy, UpgradePlanSHA256: managed.PlanSHA256(release.UpgradePlan), LayoutChange: config.Layout != LayoutBlueGreen, PendingMigrations: report.PendingMigrations, Steps: plan.Steps}
	identity := struct {
		Summary                                    update.PlanSummary
		App, Web, Compose, Root, Slot, Environment string
	}{summary, release.AppImage, release.WebImage, managed.PlanSHA256(release.ComposeTemplate), config.Root, config.ActiveSlot, ""}
	environment, err := os.ReadFile(filepath.Join(config.Root, ".env.prod"))
	if err != nil {
		return summary, err
	}
	identity.Environment = managed.PlanSHA256(environment)
	contents, err := json.Marshal(identity)
	if err != nil {
		return summary, err
	}
	summary.PlanSHA256 = managed.PlanSHA256(contents)
	return summary, nil
}

func (service *Service) ExecuteRelease(ctx context.Context, id string, release managed.Release, options update.Options, observe update.Observer) update.Result {
	result := update.Result{Status: update.StatusFailed, Target: release}
	failBefore := func(err error) update.Result {
		result.Error = err.Error()
		if observe != nil {
			_ = observe(update.Stage{Name: "preflight", Status: "failed", Message: err.Error(), UpdatedAt: time.Now().UTC()})
		}
		return result
	}
	if !safeOperationID(options.OperationID) {
		return failBefore(errors.New("an operation identity is required for a planned update"))
	}
	config, err := service.loadConfig(id)
	if err != nil {
		return failBefore(err)
	}
	if err := service.Preflight(ctx, id, release); err != nil {
		return failBefore(err)
	}
	if existing, err := service.readTransaction(id); err == nil && (existing.Status == update.StatusRecoveryRequired || existing.Status == "running") {
		return failBefore(errors.New("the previous deployment requires recovery"))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return failBefore(err)
	}
	slot := "blue"
	if config.Layout == LayoutBlueGreen {
		slot = otherSlot(config.ActiveSlot)
	}
	directory := filepath.Join(service.instanceDirectory(id), "slots", slot)
	if err := service.ensureSlotFree(ctx, config, slot); err != nil {
		return failBefore(err)
	}
	sourceVersion, err := os.ReadFile(filepath.Join(config.Root, "version.json"))
	if err != nil {
		return failBefore(err)
	}
	// Keep the retained slot intact until inspection and plan binding succeed.
	// Recovery may need this preparation directory once the transaction references it.
	preparation, err := os.MkdirTemp(service.instanceDirectory(id), ".release-")
	if err != nil {
		return failBefore(err)
	}
	preparationReferenced := false
	defer func() {
		if !preparationReferenced {
			_ = os.RemoveAll(preparation)
		}
	}()
	prepareSlot := slot
	if config.Layout != LayoutBlueGreen {
		prepareSlot = ""
	}
	candidate, err := service.prepareRelease(ctx, config, release, preparation, prepareSlot)
	if err != nil {
		return failBefore(err)
	}
	if err := service.command(ctx, candidate, "pull"); err != nil {
		return failBefore(err)
	}
	report, err := service.applicationPhase(ctx, candidate, release, config.ReleaseSequence, options.OperationID, "inspect", false)
	if err != nil {
		return failBefore(err)
	}
	summary, err := service.summarize(config, release, report)
	if err != nil {
		return failBefore(err)
	}
	if summary.Strategy == managed.StrategyMaintenance && !options.AllowMaintenance {
		return failBefore(errors.New("this plan requires a maintenance window; explicitly allow maintenance to execute"))
	}
	if options.ExpectedPlanSHA256 != "" && options.ExpectedPlanSHA256 != summary.PlanSHA256 {
		return failBefore(errors.New("upgrade plan changed after preview"))
	}

	tx := releaseTransaction{SchemaVersion: 1, OperationID: options.OperationID, Status: update.Status("running"), Source: config, Candidate: candidate, Target: release, SourceVersion: sourceVersion, Strategy: summary.Strategy, PlanSHA256: summary.PlanSHA256}
	if !validSlot(tx.Candidate.ActiveSlot) {
		tx.Candidate.ActiveSlot = slot
	}
	// A rename can commit before the parent-directory sync reports an error.
	// Retain preparation on every ambiguous journal write for crash recovery.
	preparationReferenced = true
	if err := service.saveTransaction(&tx); err != nil {
		if saved, readErr := service.readTransaction(id); readErr == nil && saved.OperationID == tx.OperationID {
			return update.Result{Status: update.StatusRecoveryRequired, Target: release, Error: err.Error()}
		}
		return failBefore(err)
	}
	step := func(name string, run func() error) error {
		tx.Stage = name
		if err := service.saveTransaction(&tx); err != nil {
			return err
		}
		if observe != nil {
			if err := observe(update.Stage{Name: name, Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		if err := run(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := service.saveTransaction(&tx); err != nil {
			return err
		}
		if observe != nil {
			return observe(update.Stage{Name: name, Status: "succeeded", UpdatedAt: time.Now().UTC()})
		}
		return nil
	}
	run := func() error {
		if config.Layout == LayoutBlueGreen {
			if err := step("pull", func() error {
				var err error
				candidate, err = service.prepareRelease(ctx, config, release, directory, slot)
				if err != nil {
					return err
				}
				tx.Candidate = candidate
				if err := service.saveTransaction(&tx); err != nil {
					return err
				}
				preparationReferenced = false
				return nil
			}); err != nil {
				return err
			}
		}
		if config.Layout != LayoutBlueGreen {
			if err := step("retain-assets", func() error { return service.copyAssets(ctx, config) }); err != nil {
				return err
			}
		}
		if tx.Strategy == managed.StrategyMaintenance {
			if err := step("quiesce", func() error { return service.Quiesce(ctx, id) }); err != nil {
				return err
			}
			if err := step("backup", func() error {
				var err error
				tx.RecoveryPointID, err = service.CreateRecoveryPoint(ctx, id, "update-to-"+release.Version)
				return err
			}); err != nil {
				return err
			}
			if err := service.startServices(ctx, infrastructureConfig(config), "redis"); err != nil {
				return err
			}
			if err := service.command(ctx, infrastructureConfig(config), "up", "-d", "--no-deps", "--wait", "redis"); err != nil {
				return err
			}
		} else {
			if err := step("online-backup", func() error { return service.onlineBackup(ctx, &tx) }); err != nil {
				return err
			}
		}
		if err := step("upgrade", func() error {
			_, err := service.applicationPhase(ctx, candidate, release, config.ReleaseSequence, options.OperationID, "apply", tx.Strategy == managed.StrategyMaintenance)
			return err
		}); err != nil {
			return err
		}
		if config.Layout != LayoutBlueGreen || service.verifyInfrastructurePins(config, release) != nil {
			if err := step("layout", func() error {
				var err error
				candidate, err = service.prepareRelease(ctx, config, release, directory, slot)
				if err != nil {
					return err
				}
				candidate.Layout = LayoutBlueGreen
				candidate.InfraComposeFile = filepath.Join(service.instanceDirectory(id), "infra", "docker-compose.yml")
				candidate.InfraEnvironmentFile = filepath.Join(service.instanceDirectory(id), "infra", "release.env")
				tx.Candidate = candidate
				tx.LayoutStarted = true
				if err := service.saveTransaction(&tx); err != nil {
					return err
				}
				preparationReferenced = false
				return service.installInfrastructure(ctx, config, candidate, release)
			}); err != nil {
				return err
			}
		}
		candidate = tx.Candidate
		if err := step("candidate", func() error {
			if err := service.prepareViews(candidate); err != nil {
				return err
			}
			if err := service.startServices(ctx, candidate, "app", "reverb", "web"); err != nil {
				return err
			}
			if tx.Strategy == managed.StrategyMaintenance {
				if err := service.command(ctx, candidate, "run", "--rm", "--no-deps", "--entrypoint", "php", "init", "artisan", "up"); err != nil {
					return err
				}
			}
			if _, err := service.applicationPhase(ctx, candidate, release, config.ReleaseSequence, options.OperationID, "verify", tx.Strategy == managed.StrategyMaintenance); err != nil {
				return err
			}
			if err := service.waitHTTP(ctx, candidate); err != nil {
				return err
			}
			return service.copyAssets(ctx, candidate)
		}); err != nil {
			return err
		}
		if err := step("switch", func() error {
			tx.TrafficOpened = true
			if err := service.saveTransaction(&tx); err != nil {
				return err
			}
			workers, err := service.switchIngress(ctx, candidate, release.Sequence, config.Layout == LayoutBlueGreen)
			tx.OldIngressWorkers = workers
			if err != nil {
				return err
			}
			return service.activateSlot(candidate, release.VersionDocument)
		}); err != nil {
			return err
		}
		if err := step("workers", func() error {
			if config.Layout == LayoutBlueGreen {
				if err := service.drainBackground(ctx, config); err != nil {
					return err
				}
			}
			names, err := backgroundServices(candidate.ComposeFile)
			if err != nil {
				return err
			}
			if err := service.startServices(ctx, candidate, names...); err != nil {
				return err
			}
			tx.WorkersHandedOver = true
			return nil
		}); err != nil {
			return err
		}
		if err := step("observe", func() error { return service.observeRelease(ctx, candidate, release) }); err != nil {
			return err
		}
		if config.Layout == LayoutBlueGreen {
			if err := step("drain", func() error {
				if err := service.drainServices(ctx, config, "reverb"); err != nil {
					return err
				}
				if err := service.waitIngressWorkers(ctx, candidate, tx.OldIngressWorkers); err != nil {
					return err
				}
				return service.drainServices(ctx, config, "web", "reverb", "app")
			}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := run(); err != nil {
		return service.failRelease(ctx, &tx, err, observe)
	}
	tx.Status = update.StatusSucceeded
	tx.Stage = "succeeded"
	if err := service.saveTransaction(&tx); err != nil {
		return service.failRelease(ctx, &tx, err, observe)
	}
	if observe != nil {
		if err := observe(update.Stage{Name: "succeeded", Status: "succeeded", UpdatedAt: time.Now().UTC()}); err != nil {
			return service.failRelease(ctx, &tx, err, observe)
		}
	}
	return update.Result{Status: update.StatusSucceeded, Target: release, RecoveryPointID: tx.RecoveryPointID}
}

func (service *Service) failRelease(ctx context.Context, tx *releaseTransaction, cause error, observe update.Observer) update.Result {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Minute)
	defer cancel()
	tx.Error = cause.Error()
	tx.Status = update.StatusRecoveryRequired
	_ = service.saveTransaction(tx)
	if observe != nil {
		_ = observe(update.Stage{Name: tx.Stage, Status: "failed", Message: cause.Error(), UpdatedAt: time.Now().UTC()})
	}
	var err error
	// A killed Docker client leaves its one-off migration container running.
	// Never restore data or reopen the source while that process can still write.
	if err = service.waitUpgradeContainers(recoveryCtx, tx.OperationID); err != nil {
		tx.Error = errors.Join(cause, err).Error()
		_ = service.saveTransaction(tx)
		return update.Result{Status: tx.Status, Target: tx.Target, RecoveryPointID: tx.RecoveryPointID, Error: tx.Error}
	}
	if tx.Strategy == managed.StrategyOnline {
		// A drain deadline retains both slots so live requests are never killed to finish a release.
		if tx.Stage == "drain" {
			err = errors.New("old connections are still draining; both slots were retained")
		} else {
			err = service.restoreApplication(recoveryCtx, tx)
			if err == nil {
				tx.Status = update.StatusRolledBack
			}
		}
	} else if tx.TrafficOpened {
		err = errors.New("traffic or background writes may have resumed; data restoration requires a separate authorized recovery")
	} else if tx.RecoveryPointID != "" {
		err = service.restoreBeforeTraffic(recoveryCtx, tx)
		if err == nil {
			tx.Status = update.StatusRolledBack
		}
	} else {
		err = service.Resume(recoveryCtx, tx.Source.ID)
		if err == nil {
			tx.Status = update.StatusFailed
		}
	}
	if err != nil {
		tx.Error = errors.Join(cause, err).Error()
	}
	if saveErr := service.saveTransaction(tx); saveErr != nil {
		tx.Status = update.StatusRecoveryRequired
		tx.Error = errors.Join(errors.New(tx.Error), saveErr).Error()
	}
	return update.Result{Status: tx.Status, Target: tx.Target, RecoveryPointID: tx.RecoveryPointID, Error: tx.Error}
}

func (service *Service) ReconcileRelease(ctx context.Context, id, operationID string) (update.Result, bool) {
	tx, err := service.readTransaction(id)
	if errors.Is(err, os.ErrNotExist) {
		return update.Result{}, false
	}
	if err != nil {
		return update.Result{Status: update.StatusRecoveryRequired, Error: err.Error()}, true
	}
	if tx.OperationID != operationID {
		return update.Result{}, false
	}
	if tx.Status == update.StatusSucceeded || tx.Status == update.StatusRolledBack || tx.Status == update.StatusFailed {
		return update.Result{Status: tx.Status, Target: tx.Target, RecoveryPointID: tx.RecoveryPointID, Error: tx.Error}, true
	}
	if tx.Strategy == managed.StrategyOnline && tx.Stage == "drain" {
		if err := service.drainServices(ctx, tx.Source, "reverb"); err != nil {
			return service.failRelease(ctx, &tx, err, nil), true
		}
		if err := service.waitIngressWorkers(ctx, tx.Candidate, tx.OldIngressWorkers); err == nil {
			if err := service.drainServices(ctx, tx.Source, "web", "reverb", "app"); err == nil {
				tx.Status = update.StatusSucceeded
				tx.Stage = "succeeded"
				tx.Error = ""
				if err := service.saveTransaction(&tx); err == nil {
					return update.Result{Status: tx.Status, Target: tx.Target}, true
				}
			}
		}
	}
	return service.failRelease(ctx, &tx, errors.New("updater interrupted during "+tx.Stage), nil), true
}

func (service *Service) activateSlot(config instance.Config, version []byte) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := replaceContents(filepath.Join(config.Root, "version.json"), version, 0640); err != nil {
		return err
	}
	return replaceContents(filepath.Join(service.instanceDirectory(config.ID), "instance.yml"), data, 0640)
}
func backgroundServices(path string) ([]string, error) {
	names, err := applicationServices(path)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, name := range names {
		if name != "app" && name != "web" && name != "reverb" {
			out = append(out, name)
		}
	}
	return out, nil
}
func (service *Service) drainBackground(ctx context.Context, config instance.Config) error {
	drainCtx, cancel := context.WithTimeout(ctx, 17*time.Minute)
	defer cancel()
	if err := service.drainScheduler(drainCtx, config); err != nil {
		return err
	}
	names, err := backgroundServices(config.ComposeFile)
	if err != nil {
		return err
	}
	workers := []string{}
	for _, name := range names {
		if name != "scheduler" {
			workers = append(workers, name)
		}
	}
	return service.drainServices(drainCtx, config, workers...)
}
func (service *Service) waitHTTP(ctx context.Context, config instance.Config) error {
	deadline, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	for {
		if err := service.checkHTTP(deadline, config); err == nil {
			return nil
		}
		if err := waitContext(deadline, time.Second); err != nil {
			return err
		}
	}
}
func (service *Service) observeRelease(ctx context.Context, config instance.Config, release managed.Release) error {
	duration := service.ObservationDuration
	if duration <= 0 {
		duration = 120 * time.Second
	}
	deadline := time.Now().Add(duration)
	for {
		if err := service.checkHTTP(ctx, config); err != nil {
			return err
		}
		if err := service.checkIngressIdentity(ctx, config); err != nil {
			return err
		}
		if err := service.runningServices(ctx, config); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		if err := waitContext(ctx, time.Second); err != nil {
			return err
		}
	}
}
func (service *Service) prepareViews(config instance.Config) error {
	path := filepath.Join(service.instanceDirectory(config.ID), "slots", config.ActiveSlot, "views")
	if err := safeDirectory(path, 0755); err != nil {
		return err
	}
	if service.Chown != nil {
		return service.Chown(path, 33, 33)
	}
	return os.Chown(path, 33, 33)
}
func (service *Service) ensureSlotFree(ctx context.Context, config instance.Config, slot string) error {
	if !validSlot(slot) || slot == config.ActiveSlot {
		return errors.New("candidate slot is not inactive")
	}
	project := "geoflow-" + config.ID + "-" + slot
	var out limitedBuffer
	if err := service.runner().Run(ctx, nil, &out, "docker", "ps", "--filter", "label=com.docker.compose.project="+project, "--format={{.ID}}"); err != nil {
		return err
	}
	if strings.TrimSpace(out.String()) != "" {
		return errors.New("inactive slot still has running containers; finish draining before reusing it")
	}
	return nil
}
func (service *Service) verifyInfrastructurePins(config instance.Config, release managed.Release) error {
	contents, err := os.ReadFile(config.InfraEnvironmentFile)
	if err != nil {
		return err
	}
	pg, redis, err := release.InfrastructureImages(config.PostgresMajor, config.RedisMajor)
	if err != nil {
		return err
	}
	for key, want := range map[string]string{"GEOFLOW_POSTGRES_IMAGE": pg, "GEOFLOW_REDIS_IMAGE": redis} {
		got, err := environmentValue(contents, key)
		if err != nil {
			return err
		}
		if got != want {
			return errors.New("infrastructure image changes require a separate maintenance infrastructure operation")
		}
	}
	return nil
}
func (service *Service) onlineBackup(ctx context.Context, tx *releaseTransaction) error {
	directory := filepath.Join(service.instanceDirectory(tx.Source.ID), "online-backups", tx.OperationID)
	if err := safeDirectory(directory, 0700); err != nil {
		return err
	}
	path := filepath.Join(directory, "postgres.dump")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	dumpErr := (postgresDatabase{config: tx.Source, runner: service.runner()}).Dump(ctx, file)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(dumpErr, syncErr, closeErr); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return errors.New("online database dump is empty")
	}
	check, err := os.Open(path)
	if err != nil {
		return err
	}
	defer check.Close()
	if err := service.runner().Run(ctx, check, io.Discard, "docker", append(composeArguments(tx.Source.Root, infrastructureConfig(tx.Source).EnvironmentFile, infrastructureConfig(tx.Source).ComposeFile), "exec", "-T", "postgres", "pg_restore", "--list")...); err != nil {
		return fmt.Errorf("online dump verification: %w", err)
	}
	if _, err := check.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, check); err != nil {
		return err
	}
	metadata := map[string]any{"sha256": hex.EncodeToString(digest.Sum(nil)), "schema_version": 1, "consistency": "postgresql-snapshot-only", "automatic_data_restore": false, "source_sequence": tx.Source.ReleaseSequence, "created_at": time.Now().UTC(), "bytes": info.Size()}
	data, _ := json.Marshal(metadata)
	if err := replaceContents(filepath.Join(directory, "metadata.json"), data, 0600); err != nil {
		return err
	}
	return pruneOnlineBackups(filepath.Dir(directory), tx.OperationID, 5)
}

func pruneOnlineBackups(root, current string, keep int) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	type saved struct {
		name string
		at   time.Time
	}
	var others []saved
	for _, entry := range entries {
		if entry.Name() == current || !entry.IsDir() || !safeOperationID(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		others = append(others, saved{entry.Name(), info.ModTime()})
	}
	sort.Slice(others, func(i, j int) bool { return others[i].at.After(others[j].at) })
	for i := keep - 1; i < len(others); i++ {
		if i >= 0 {
			if err := os.RemoveAll(filepath.Join(root, others[i].name)); err != nil {
				return err
			}
		}
	}
	return nil
}
