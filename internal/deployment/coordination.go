package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

// ActionSnapshot runs under the instance operation lock at planning and again
// at admission. A release selected here is pinned for execution after admission.
func (service *Service) ActionSnapshot(ctx context.Context, id, action, pointID string) (coordination.Snapshot, error) {
	var result coordination.Snapshot
	authority, err := (recoverycontrol.Store{StateDir: service.StateDir}).Read(id)
	if err != nil {
		return result, err
	}
	if authority.State.Phase != "ready" && action != "restore" {
		return result, errors.New("recovery reconciliation is required")
	}
	config, err := service.loadConfig(id)
	if err != nil {
		return result, err
	}
	hashes := map[string]string{}
	paths := []string{filepath.Join(service.instanceDirectory(id), "instance.yml"), config.ComposeFile, config.EnvironmentFile, filepath.Join(config.Root, ".env.prod"), filepath.Join(config.Root, "version.json")}
	if config.InfraComposeFile != "" {
		paths = append(paths, config.InfraComposeFile, config.InfraEnvironmentFile, filepath.Join(filepath.Dir(config.InfraComposeFile), "traffic", "nginx.conf"), filepath.Join(filepath.Dir(config.ComposeFile), "upgrade-plan.json"), filepath.Join(filepath.Dir(config.ComposeFile), "version.json"))
	}
	for _, path := range paths {
		if err := regularFile(path); err != nil {
			return result, err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return result, err
		}
		hashes[path] = managed.PlanSHA256(contents)
	}
	baseline := map[string]any{"config": config, "files": hashes, "recovery": authority.State, "action": action}
	details := map[string]any{"source": map[string]any{"version": config.Version, "sequence": config.ReleaseSequence, "slot": config.ActiveSlot}}
	result.Continuation = "host_only" // No existing signed target declares remote receipt compatibility.
	switch action {
	case "update":
		release, err := service.Resolve(ctx, id)
		if err != nil {
			return result, err
		}
		summary, err := service.previewRelease(ctx, id, release)
		if err != nil {
			return result, err
		}
		result.Target = &release
		result.UpdatePlanSHA256 = summary.PlanSHA256
		result.MaintenanceRequired = summary.Strategy == managed.StrategyMaintenance
		baseline["target"] = release
		baseline["plan"] = summary
		details["target"] = map[string]any{"version": release.Version, "sequence": release.Sequence, "app_image": release.AppImage, "web_image": release.WebImage}
		details["upgrade"] = summary
	case "backup":
		result.MaintenanceRequired = true
		baseline["scope"] = []string{"database", "storage", "configuration", "deployment", "redis"}
		details["scope"] = baseline["scope"]
	case "restore":
		points, err := service.ListRecoveryPoints(id)
		if err != nil {
			return result, err
		}
		found := false
		for _, point := range points {
			if !point.IsUpdateCheckpoint() {
				continue
			}
			if point.ID != pointID {
				return result, errors.New("restore requires latest update checkpoint")
			}
			if err := service.ValidateRecoveryPoint(id, pointID); err != nil {
				return result, err
			}
			baseline["point"] = point
			baseline["scope"] = []string{"database", "storage", "configuration", "deployment", "redis"}
			details["recovery_point"] = map[string]any{"id": point.ID, "version": point.Version, "sequence": point.ReleaseSequence, "created_at": point.CreatedAt, "manifest_sha256": coordination.Digest(point)}
			found = true
			break
		}
		if !found {
			return result, errors.New("latest update checkpoint is unavailable")
		}
		result.MaintenanceRequired = true
	case "switch-back":
		tx, err := service.readTransaction(id)
		if err != nil {
			return result, err
		}
		if tx.Strategy != managed.StrategyOnline || tx.Source.Layout != LayoutBlueGreen || tx.Status != update.StatusSucceeded || config.ReleaseSequence != tx.Target.Sequence || config.ActiveSlot != tx.Candidate.ActiveSlot {
			return result, errors.New("compatible retained release is unavailable")
		}
		retained := map[string]string{}
		for _, path := range []string{tx.Source.ComposeFile, tx.Source.EnvironmentFile} {
			if err := regularFile(path); err != nil {
				return result, err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return result, err
			}
			retained[path] = managed.PlanSHA256(b)
		}
		baseline["transaction"] = tx
		baseline["retained_files"] = retained
		details["target"] = map[string]any{"version": tx.Source.Version, "sequence": tx.Source.ReleaseSequence, "slot": tx.Source.ActiveSlot}
		details["deployment_transaction_id"] = tx.OperationID
	default:
		return result, errors.New("invalid action")
	}
	result.BaselineSHA256 = coordination.Digest(baseline)
	result.Details, err = json.Marshal(details)
	return result, err
}
