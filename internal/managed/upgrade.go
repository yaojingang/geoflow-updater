package managed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
)

const (
	StrategyMaintenance = "maintenance"
	StrategyOnline      = "online"
	UpgradePlanLimit    = 1024 * 1024
)

var migrationNamePattern = regexp.MustCompile(`^[0-9]{4}_[0-9]{2}_[0-9]{2}_[0-9]{6}_[a-zA-Z0-9_]+$`)
var planStepPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type UpgradeMigration struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Online bool   `json:"online"`
}

type UpgradeStep struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Phase          string `json:"phase"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Online         bool   `json:"online"`
}

type Compatibility struct {
	Schema  bool `json:"schema"`
	Queue   bool `json:"queue"`
	Cache   bool `json:"cache"`
	Storage bool `json:"storage"`
}

func (value Compatibility) Online() bool {
	return value.Schema && value.Queue && value.Cache && value.Storage
}

type UpgradePlan struct {
	SchemaVersion  int                `json:"schema_version"`
	Strategy       string             `json:"strategy"`
	AllowedSources []uint64           `json:"allowed_sources"`
	Migrations     []UpgradeMigration `json:"migrations"`
	Compatibility  Compatibility      `json:"compatibility"`
	Steps          []UpgradeStep      `json:"steps"`
}

func DecodeUpgradePlan(contents []byte) (UpgradePlan, error) {
	var plan UpgradePlan
	if len(contents) == 0 || len(contents) > UpgradePlanLimit {
		return plan, errors.New("upgrade plan is missing or too large")
	}
	if err := validateUpgradePlanFields(contents); err != nil {
		return plan, fmt.Errorf("decode upgrade plan: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("decode upgrade plan: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, errors.New("upgrade plan contains trailing JSON")
	}
	if err := plan.Validate(); err != nil {
		return plan, err
	}
	return plan, nil
}

// Check the wire contract before decoding into Go value types. Missing fields,
// null values and case aliases must not acquire valid defaults during decoding.
func validateUpgradePlanFields(contents []byte) error {
	fields, err := upgradePlanObject(contents, "schema_version", "strategy", "allowed_sources", "migrations", "compatibility", "steps")
	if err != nil {
		return err
	}
	if _, err := upgradePlanObject(fields["compatibility"], "schema", "queue", "cache", "storage"); err != nil {
		return fmt.Errorf("compatibility: %w", err)
	}
	for name, keys := range map[string][]string{
		"migrations": {"name", "sha256", "online"},
		"steps":      {"id", "kind", "phase", "timeout_seconds", "online"},
	} {
		var entries []json.RawMessage
		if err := json.Unmarshal(fields[name], &entries); err != nil {
			return fmt.Errorf("%s must be an array: %w", name, err)
		}
		for _, entry := range entries {
			if _, err := upgradePlanObject(entry, keys...); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

func upgradePlanObject(contents []byte, keys ...string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("expected an object")
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] {
			return nil, errors.New("object contains an unknown field")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("field %q cannot be null", key)
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("object is incomplete")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("object contains trailing JSON")
	}
	if len(fields) != len(keys) {
		return nil, errors.New("object is missing required fields")
	}
	return fields, nil
}

func (plan UpgradePlan) Validate() error {
	if plan.SchemaVersion != 1 || (plan.Strategy != StrategyMaintenance && plan.Strategy != StrategyOnline) {
		return errors.New("upgrade plan schema or strategy is unsupported")
	}
	sources := map[uint64]bool{}
	for _, source := range plan.AllowedSources {
		if source == 0 || source > math.MaxInt64 || sources[source] {
			return errors.New("upgrade plan source sequences must be unique positive application integers")
		}
		sources[source] = true
	}
	if plan.Strategy == StrategyOnline && (len(sources) == 0 || !plan.Compatibility.Online()) {
		return errors.New("online plan requires explicit source releases and data compatibility")
	}
	migrations := map[string]bool{}
	for _, migration := range plan.Migrations {
		if !migrationNamePattern.MatchString(migration.Name) || !digestPattern.MatchString(migration.SHA256) || migrations[migration.Name] {
			return errors.New("upgrade plan migration identity is invalid or duplicated")
		}
		migrations[migration.Name] = true
	}
	if len(plan.Steps) < 1 || len(plan.Steps) > 32 {
		return errors.New("upgrade plan requires between one and 32 steps")
	}
	steps := map[string]bool{}
	kinds := map[string]bool{}
	for _, step := range plan.Steps {
		if !planStepPattern.MatchString(step.ID) || steps[step.ID] || kinds[step.Kind] || step.Phase != "apply" || step.TimeoutSeconds < 1 || step.TimeoutSeconds > 7200 {
			return errors.New("upgrade step identity, phase, or timeout is invalid")
		}
		switch step.Kind {
		case "migrate", "retrieval_backfill", "managed_images", "security_audit", "system_knowledge", "cache_warmup":
		default:
			return errors.New("upgrade plan contains an unsupported step kind")
		}
		if plan.Strategy == StrategyOnline && !step.Online {
			return errors.New("online plan contains a maintenance step")
		}
		steps[step.ID] = true
		kinds[step.Kind] = true
	}
	if plan.Steps[0].Kind != "migrate" {
		return errors.New("upgrade plan must begin with the migration step")
	}
	return nil
}

func (plan UpgradePlan) AllowsSource(sequence uint64) bool {
	if plan.Strategy == StrategyMaintenance && len(plan.AllowedSources) == 0 {
		return true
	}
	for _, allowed := range plan.AllowedSources {
		if sequence == allowed {
			return true
		}
	}
	return false
}

func PlanSHA256(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func (release Release) Plan() (UpgradePlan, error) { return DecodeUpgradePlan(release.UpgradePlan) }
