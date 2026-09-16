package coordination

import (
	"encoding/json"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

// Snapshot is private host evidence; only Details is exposed to clients.
type Snapshot struct {
	BaselineSHA256      string           `json:"baseline_sha256"`
	MaintenanceRequired bool             `json:"maintenance_required"`
	Continuation        string           `json:"continuation"`
	Details             json.RawMessage  `json:"details"`
	Target              *managed.Release `json:"target,omitempty"`
	UpdatePlanSHA256    string           `json:"update_plan_sha256,omitempty"`
}
