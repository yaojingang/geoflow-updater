package update

import (
	"context"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type Options struct {
	LegacyRequest      bool   `json:"-"`
	AllowMaintenance   bool   `json:"allow_maintenance"`
	ExpectedPlanSHA256 string `json:"expected_plan_sha256,omitempty"`
	OperationID        string `json:"-"`
}

type PlanSummary struct {
	SchemaVersion     int                        `json:"schema_version"`
	SourceSequence    uint64                     `json:"source_sequence"`
	TargetSequence    uint64                     `json:"target_sequence"`
	TargetVersion     string                     `json:"target_version"`
	Strategy          string                     `json:"strategy"`
	PlanSHA256        string                     `json:"plan_sha256"`
	UpgradePlanSHA256 string                     `json:"upgrade_plan_sha256"`
	LayoutChange      bool                       `json:"layout_change"`
	PendingMigrations []managed.UpgradeMigration `json:"pending_migrations"`
	Steps             []managed.UpgradeStep      `json:"steps"`
}

type PlannedDeployment interface {
	ExecuteRelease(context.Context, string, managed.Release, Options, Observer) Result
}

const StatusRecoveryRequired Status = "recovery_required"
