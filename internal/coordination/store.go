// Package coordination owns durable plans and admission receipts outside business snapshots.
package coordination

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	ErrConflict     = errors.New("request_conflict")
	ErrPlan         = errors.New("plan_invalid")
	ErrBaseline     = errors.New("baseline_changed")
	ErrEpoch        = errors.New("epoch_changed")
	ErrConfirmation = errors.New("confirmation_required")
	identifier      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
	hex32           = regexp.MustCompile(`^[a-f0-9]{32}$`)
	hex64           = regexp.MustCompile(`^[a-f0-9]{64}$`)
	uuid            = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	opID            = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[a-f0-9]{16}$`)
	pointID         = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-f0-9]{8}$`)
)

type Actor struct {
	ManagementInstanceID string `json:"management_instance_id"`
	AdminID              int64  `json:"admin_id"`
	IdentitySHA256       string `json:"identity_sha256"`
}

func (a Actor) Valid() bool {
	return uuid.MatchString(a.ManagementInstanceID) && a.AdminID > 0 && hex64.MatchString(a.IdentitySHA256)
}
func ValidRequestID(id string) bool   { return identifier.MatchString(id) }
func ValidOperationID(id string) bool { return opID.MatchString(id) }
func Scope(action string) string {
	switch action {
	case "update", "switch-back":
		return "update"
	case "backup":
		return "backup"
	case "restore":
		return "rollback"
	}
	return ""
}
func WireScope(action string) string {
	if action == "restore" {
		return "updater:restore"
	}
	if scope := Scope(action); scope != "" {
		return "updater:" + scope
	}
	return ""
}

type PlanRequest struct {
	Action          string `json:"action"`
	ExpectedEpoch   string `json:"expected_epoch"`
	Actor           Actor  `json:"actor"`
	RecoveryPointID string `json:"recovery_point_id,omitempty"`
}

func (r PlanRequest) Valid() bool {
	return Scope(r.Action) != "" && hex32.MatchString(r.ExpectedEpoch) && r.Actor.Valid() && ((r.Action == "restore" && pointID.MatchString(r.RecoveryPointID)) || (r.Action != "restore" && r.RecoveryPointID == ""))
}

type Plan struct {
	SchemaVersion       int             `json:"schema_version"`
	InstanceID          string          `json:"instance_id"`
	PlanID              string          `json:"plan_id"`
	PlanSHA256          string          `json:"plan_sha256"`
	BaselineSHA256      string          `json:"baseline_sha256"`
	ExpectedEpoch       string          `json:"expected_epoch"`
	Action              string          `json:"action"`
	Actor               Actor           `json:"actor"`
	ExpiresAt           time.Time       `json:"expires_at"`
	MaintenanceRequired bool            `json:"maintenance_required"`
	Continuation        string          `json:"continuation"`
	RecoveryPointID     string          `json:"recovery_point_id,omitempty"`
	Details             json.RawMessage `json:"details,omitempty"`
	Snapshot            json.RawMessage `json:"snapshot,omitempty"`
}

func (p Plan) Digest() string { p.PlanSHA256 = ""; return Digest(p) }
func (p Plan) Public() Plan   { p.Snapshot = nil; return p }
func Digest(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (p Plan) valid() bool {
	return p.SchemaVersion == 2 && p.InstanceID == "primary" && hex32.MatchString(p.PlanID) && hex64.MatchString(p.PlanSHA256) && hex64.MatchString(p.BaselineSHA256) && hex32.MatchString(p.ExpectedEpoch) && Scope(p.Action) != "" && p.Actor.Valid() && !p.ExpiresAt.IsZero() && (p.Continuation == "remote" || p.Continuation == "host_only") && (p.RecoveryPointID == "" || pointID.MatchString(p.RecoveryPointID)) && p.Digest() == p.PlanSHA256
}

type SubmitRequest struct {
	Action            string `json:"action"`
	PlanID            string `json:"plan_id"`
	PlanSHA256        string `json:"plan_sha256"`
	ExpectedEpoch     string `json:"expected_epoch"`
	AllowMaintenance  bool   `json:"allow_maintenance"`
	ConfirmHostAccess bool   `json:"confirm_host_access"`
	ClientRequestID   string `json:"client_request_id"`
	Actor             Actor  `json:"actor"`
	Scope             string `json:"scope"`
}

func (r SubmitRequest) Valid() bool {
	return Scope(r.Action) != "" && hex32.MatchString(r.PlanID) && hex64.MatchString(r.PlanSHA256) && hex32.MatchString(r.ExpectedEpoch) && identifier.MatchString(r.ClientRequestID) && r.Actor.Valid() && r.Scope == WireScope(r.Action)
}
func (r SubmitRequest) BusinessSHA256() string {
	return Digest(map[string]any{"action": r.Action, "plan_id": r.PlanID, "plan_sha256": r.PlanSHA256, "expected_epoch": r.ExpectedEpoch, "allow_maintenance": r.AllowMaintenance, "confirm_host_access": r.ConfirmHostAccess})
}

type Admission struct {
	Sequence        uint64          `json:"sequence"`
	SchemaVersion   int             `json:"schema_version"`
	InstanceID      string          `json:"instance_id"`
	ClientRequestID string          `json:"client_request_id"`
	BusinessSHA256  string          `json:"business_sha256"`
	OperationID     string          `json:"operation_id"`
	Action          string          `json:"action"`
	PlanID          string          `json:"plan_id"`
	AcceptedEpoch   string          `json:"accepted_epoch"`
	Actor           Actor           `json:"actor"`
	Scope           string          `json:"scope"`
	Counter         int64           `json:"counter"`
	AcceptedAt      time.Time       `json:"accepted_at"`
	RecoveryPointID string          `json:"recovery_point_id,omitempty"`
	Operation       json.RawMessage `json:"operation"`
}

func (a Admission) valid() bool {
	return a.Sequence > 0 && a.SchemaVersion == 2 && a.InstanceID == "primary" && identifier.MatchString(a.ClientRequestID) && hex64.MatchString(a.BusinessSHA256) && opID.MatchString(a.OperationID) && hex32.MatchString(a.PlanID) && hex32.MatchString(a.AcceptedEpoch) && a.Actor.Valid() && a.Scope == Scope(a.Action) && a.Scope != "" && a.Counter >= 0 && !a.AcceptedAt.IsZero() && json.Valid(a.Operation) && (a.RecoveryPointID == "" || pointID.MatchString(a.RecoveryPointID))
}

type Receipt struct {
	SchemaVersion    int             `json:"schema_version"`
	InstanceID       string          `json:"instance_id"`
	ClientRequestID  string          `json:"client_request_id"`
	BusinessSHA256   string          `json:"business_sha256"`
	OperationID      string          `json:"operation_id"`
	Action           string          `json:"action"`
	PlanID           string          `json:"plan_id"`
	AcceptedEpoch    string          `json:"accepted_epoch"`
	AdmissionStatus  string          `json:"admission_status"`
	BackgroundStatus string          `json:"background_status"`
	Operation        json.RawMessage `json:"operation"`
}

func (a Admission) Receipt(background string) Receipt {
	receipt := Receipt{2, a.InstanceID, a.ClientRequestID, a.BusinessSHA256, a.OperationID, a.Action, a.PlanID, a.AcceptedEpoch, "accepted", background, a.Operation}
	var projection struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(a.Operation, &projection)
	// A queued authority may precede projection completion or follow an uncertain
	// directory sync. Expose its reserved identity without claiming dispatch.
	if projection.Status == "queued" {
		receipt.AdmissionStatus = "pending"
		receipt.Operation = nil
	}
	return receipt
}

type Store struct {
	StateDir      string
	Fault         func(string) error
	SyncDirectory func(string) error
}

func (s Store) root(id, kind string) (string, error) {
	if id != "primary" {
		return "", errors.New("invalid instance")
	}
	root := s.StateDir
	if root == "" {
		root = "/var/lib/geoflow-updater"
	}
	return filepath.Join(root, "coordination", id, kind), nil
}
func (s Store) Request(id, request string) (Admission, error) {
	var a Admission
	if !identifier.MatchString(request) {
		return a, errors.New("invalid request identity")
	}
	root, err := s.root(id, "admissions")
	if err != nil {
		return a, err
	}
	if err = read(filepath.Join(root, request+".json"), &a); err != nil {
		return a, err
	}
	if !a.valid() || a.InstanceID != id || a.ClientRequestID != request {
		return a, errors.New("invalid admission authority")
	}
	return a, nil
}
func (s Store) Admissions(id string) ([]Admission, error) {
	if id != "primary" {
		return nil, nil
	}
	root, err := s.root(id, "admissions")
	if err != nil {
		return nil, err
	}
	names, err := entries(root)
	if err != nil {
		return nil, err
	}
	result := []Admission{}
	seen := map[string]bool{}
	sequences := map[uint64]bool{}
	for _, name := range names {
		a, err := s.Request(id, name)
		if err != nil {
			return nil, err
		}
		if seen[a.OperationID] {
			return nil, errors.New("duplicate operation authority")
		}
		if sequences[a.Sequence] {
			return nil, errors.New("duplicate admission sequence")
		}
		sequences[a.Sequence] = true
		seen[a.OperationID] = true
		result = append(result, a)
	}
	return result, nil
}
func (s Store) Operation(id, operation string) (Admission, error) {
	if !opID.MatchString(operation) {
		return Admission{}, errors.New("invalid operation identity")
	}
	records, err := s.Admissions(id)
	if err != nil {
		return Admission{}, err
	}
	for _, a := range records {
		if a.OperationID == operation {
			return a, nil
		}
	}
	return Admission{}, os.ErrNotExist
}
func (s Store) Counter(id, scope string) (int64, error) {
	records, err := s.Admissions(id)
	if err != nil {
		return -1, err
	}
	last := int64(-1)
	for _, a := range records {
		if a.Scope == scope && a.Counter > last {
			last = a.Counter
		}
	}
	return last, nil
}
func (s Store) Accept(a Admission) error {
	sequence, err := s.nextSequence(a.InstanceID)
	if err != nil {
		return err
	}
	a.Sequence = sequence
	if !a.valid() {
		return errors.New("invalid admission authority")
	}
	root, err := s.root(a.InstanceID, "admissions")
	if err != nil {
		return err
	}
	return s.write(filepath.Join(root, a.ClientRequestID+".json"), a, true)
}
func (s Store) UpdateOperation(id, operation string, contents json.RawMessage) error {
	latest, latestErr := s.Latest(id)
	if latestErr != nil && !errors.Is(latestErr, os.ErrNotExist) {
		return latestErr
	}
	if latestErr == nil && latest.OperationID != operation {
		return errors.New("operation was superseded by a newer operation")
	}
	a, err := s.Operation(id, operation)
	if errors.Is(err, os.ErrNotExist) {
		return s.syncHeadOperation(id, operation, contents)
	}
	if err != nil {
		return err
	}
	a.Operation = contents
	var projection struct {
		RecoveryPointID string `json:"recovery_point_id"`
	}
	if err := json.Unmarshal(contents, &projection); err != nil {
		return err
	}
	if projection.RecoveryPointID != "" {
		a.RecoveryPointID = projection.RecoveryPointID
	}
	if !a.valid() {
		return errors.New("invalid operation projection")
	}
	root, _ := s.root(id, "admissions")
	if err := s.write(filepath.Join(root, a.ClientRequestID+".json"), a, false); err != nil {
		return err
	}
	return s.syncHeadOperation(id, operation, contents)
}
func (s Store) SavePlan(p Plan) error {
	if !p.valid() {
		return ErrPlan
	}
	root, err := s.root(p.InstanceID, "plans")
	if err != nil {
		return err
	}
	return s.write(filepath.Join(root, p.PlanID+".json"), p, true)
}
func (s Store) Plan(id, plan string) (Plan, error) {
	var p Plan
	if !hex32.MatchString(plan) {
		return p, ErrPlan
	}
	root, err := s.root(id, "plans")
	if err != nil {
		return p, err
	}
	if err = read(filepath.Join(root, plan+".json"), &p); err != nil {
		return p, err
	}
	if !p.valid() || p.InstanceID != id || p.PlanID != plan {
		return p, ErrPlan
	}
	return p, nil
}
func (s Store) ProtectedPoints(id string, now time.Time) (map[string]bool, error) {
	refs := map[string]bool{}
	root, err := s.root(id, "plans")
	if err != nil {
		return nil, err
	}
	names, err := entries(root)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		p, err := s.Plan(id, name)
		if err != nil {
			return nil, err
		}
		if p.ExpiresAt.After(now) && p.RecoveryPointID != "" {
			refs[p.RecoveryPointID] = true
		}
	}
	records, err := s.Admissions(id)
	if err != nil {
		return nil, err
	}
	for _, a := range records {
		var status struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(a.Operation, &status); err != nil {
			return nil, err
		}
		if status.Status != "succeeded" && status.Status != "failed" && status.Status != "rolled_back" && a.RecoveryPointID != "" {
			refs[a.RecoveryPointID] = true
		}
	}
	return refs, nil
}
func entries(root string) ([]string, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe coordination directory")
	}
	items, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, item := range items {
		if strings.HasPrefix(item.Name(), ".pending-") {
			continue
		}
		if !strings.HasSuffix(item.Name(), ".json") {
			return nil, errors.New("unexpected coordination file")
		}
		names = append(names, strings.TrimSuffix(item.Name(), ".json"))
	}
	return names, nil
}
func read(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 1 || info.Size() > 16*1024*1024 {
		return errors.New("unsafe coordination record")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return errors.New("trailing coordination data")
	}
	return nil
}
func (s Store) fault(stage string) error {
	if s.Fault != nil {
		return s.Fault(stage)
	}
	return nil
}
func (s Store) write(path string, value any, exclusive bool) error {
	if err := s.durableDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := s.fault("before-write"); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16*1024*1024 {
		return errors.New("coordination record too large")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = s.fault("before-rename"); err != nil {
		return err
	}
	if exclusive {
		err = os.Link(file.Name(), path)
	} else {
		err = os.Rename(file.Name(), path)
	}
	if errors.Is(err, os.ErrExist) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if err = s.fault("after-rename"); err != nil {
		return err
	}
	if err = s.syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return s.fault("after-sync")
}
func (s Store) syncDirectory(path string) error {
	if s.SyncDirectory != nil {
		return s.SyncDirectory(path)
	}
	return SyncDirectory(path)
}

// Sync every component on every write. Existing directory entries may remain
// from an earlier attempt whose parent sync failed before acceptance.
func (s Store) durableDirectory(path string) error {
	boundary := s.StateDir
	if boundary == "" {
		boundary = "/var/lib/geoflow-updater"
	}
	boundary = filepath.Clean(boundary)
	relative, err := filepath.Rel(boundary, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("coordination path escaped state directory")
	}
	info, err := os.Lstat(boundary)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe state directory")
	}
	if err = s.syncDirectory(boundary); err != nil {
		return err
	}
	current := boundary
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		parent := current
		current = filepath.Join(parent, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe coordination directory")
		}
		if err = s.syncDirectory(current); err != nil {
			return err
		}
		if err = s.syncDirectory(parent); err != nil {
			return err
		}
	}
	return nil
}
func SyncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open coordination directory: %w", err)
	}
	defer f.Close()
	return f.Sync()
}
