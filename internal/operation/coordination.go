package operation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type AdmissionAuthorizer interface {
	AuthorizeAdmission(string, authorization.Scope, string, func() (bool, error), func(int64) error) error
}
type actionPlanner interface {
	ActionSnapshot(context.Context, string, string, string) (coordination.Snapshot, error)
}

func (m *Manager) coordinationStore() coordination.Store {
	if m.Coordination != nil {
		return *m.Coordination
	}
	return coordination.Store{StateDir: m.stateDir()}
}
func (m *Manager) background(id string) string {
	a, err := (recoverycontrol.Store{StateDir: m.stateDir()}).Read(id)
	if err == nil && a.State.Phase == "ready" {
		return "ready"
	}
	return "held"
}

func (m *Manager) Capabilities(id string) (map[string]any, error) {
	if id != "primary" {
		return nil, errors.New("invalid instance")
	}
	if _, ok := m.Deployment.(actionPlanner); !ok {
		return nil, errors.New("coordinated deployment is unavailable")
	}
	a, err := (recoverycontrol.Store{StateDir: m.stateDir()}).Read(id)
	if err != nil {
		return nil, err
	}
	actions := []string{"update", "backup", "restore", "switch-back"}
	if a.State.Phase != "ready" {
		actions = []string{"restore"}
	}
	return map[string]any{"schema_version": 2, "instance_id": id, "protocol_version": 2, "updater_protocol": 5, "actions": actions, "features": map[string]bool{"plans": true, "requests": true, "idempotency": true, "recovery_epoch": true}, "maintenance_confirmation": true, "restore_policy": "latest_update_checkpoint", "background_status": m.background(id), "recovery": map[string]string{"host_id": a.State.HostID, "epoch": a.State.Epoch, "phase": a.State.Phase}}, nil
}
func (m *Manager) CreatePlan(ctx context.Context, id string, request coordination.PlanRequest) (coordination.Plan, error) {
	var p coordination.Plan
	if id != "primary" || !request.Valid() {
		return p, coordination.ErrPlan
	}
	planner, ok := m.Deployment.(actionPlanner)
	if !ok {
		return p, coordination.ErrPlan
	}
	lock, err := m.acquireLock(id)
	if err != nil {
		return p, err
	}
	defer lock.Close()
	if err = m.repairAdmissions(id); err != nil {
		return p, err
	}
	current, err := m.Current(id)
	if err == nil && (current.Status == StatusQueued || current.Status == StatusRunning || (current.Status == StatusRecoveryRequired && request.Action != "restore")) {
		return p, ErrActive
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return p, err
	}
	a, err := (recoverycontrol.Store{StateDir: m.stateDir()}).Read(id)
	if err != nil {
		return p, err
	}
	if a.State.Epoch != request.ExpectedEpoch {
		return p, coordination.ErrEpoch
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	snapshot, err := planner.ActionSnapshot(ctx, id, request.Action, request.RecoveryPointID)
	if err != nil {
		return p, err
	}
	random := m.Random
	if random == nil {
		random = rand.Reader
	}
	bytes := make([]byte, 16)
	if _, err = io.ReadFull(random, bytes); err != nil {
		return p, err
	}
	private, err := json.Marshal(snapshot)
	if err != nil {
		return p, err
	}
	p = coordination.Plan{SchemaVersion: 2, InstanceID: id, PlanID: hex.EncodeToString(bytes), BaselineSHA256: snapshot.BaselineSHA256, ExpectedEpoch: a.State.Epoch, Action: request.Action, Actor: request.Actor, ExpiresAt: m.now().UTC().Add(10 * time.Minute), MaintenanceRequired: snapshot.MaintenanceRequired, Continuation: snapshot.Continuation, RecoveryPointID: request.RecoveryPointID, Details: snapshot.Details, Snapshot: private}
	p.PlanSHA256 = p.Digest()
	if err = m.coordinationStore().SavePlan(p); err != nil {
		return coordination.Plan{}, err
	}
	return p.Public(), nil
}
func (m *Manager) Plan(id, plan string) (coordination.Plan, error) {
	p, err := m.coordinationStore().Plan(id, plan)
	return p.Public(), err
}
func (m *Manager) Request(id, request string) (coordination.Receipt, error) {
	a, err := m.coordinationStore().Request(id, request)
	if err != nil {
		return coordination.Receipt{}, err
	}
	if _, err := admissionOperation(a); err != nil {
		return coordination.Receipt{}, err
	}
	return a.Receipt(m.background(id)), nil
}
func (m *Manager) OperationReceipt(id, operation string) (coordination.Receipt, error) {
	a, err := m.coordinationStore().Operation(id, operation)
	if err != nil {
		return coordination.Receipt{}, err
	}
	if _, err := admissionOperation(a); err != nil {
		return coordination.Receipt{}, err
	}
	return a.Receipt(m.background(id)), nil
}

func (m *Manager) Submit(ctx context.Context, id string, request coordination.SubmitRequest, code string, auth AdmissionAuthorizer) (coordination.Receipt, error) {
	var receipt coordination.Receipt
	if id != "primary" || !request.Valid() {
		return receipt, coordination.ErrPlan
	}
	lookup := func() (bool, error) {
		r, err := m.Request(id, request.ClientRequestID)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if r.BusinessSHA256 != request.BusinessSHA256() {
			return false, coordination.ErrConflict
		}
		receipt = r
		return true, nil
	}
	if found, err := lookup(); found || err != nil {
		return receipt, err
	}
	if auth == nil {
		return receipt, authorization.ErrUnconfigured
	}
	err := auth.AuthorizeAdmission(id, authorization.Scope(coordination.Scope(request.Action)), code, lookup, func(counter int64) error {
		var plan coordination.Plan
		var snapshot coordination.Snapshot
		validate := func() error {
			var err error
			plan, err = m.coordinationStore().Plan(id, request.PlanID)
			if err != nil {
				return coordination.ErrPlan
			}
			if plan.PlanSHA256 != request.PlanSHA256 || plan.Action != request.Action || plan.ExpectedEpoch != request.ExpectedEpoch || plan.Actor != request.Actor || !plan.ExpiresAt.After(m.now().UTC()) {
				return coordination.ErrPlan
			}
			if (plan.MaintenanceRequired && !request.AllowMaintenance) || (plan.Continuation == "host_only" && !request.ConfirmHostAccess) {
				return coordination.ErrConfirmation
			}
			a, err := (recoverycontrol.Store{StateDir: m.stateDir()}).Read(id)
			if err != nil {
				return err
			}
			if a.State.Epoch != request.ExpectedEpoch {
				return coordination.ErrEpoch
			}
			planner, ok := m.Deployment.(actionPlanner)
			if !ok {
				return coordination.ErrPlan
			}
			checkCtx, cancel := context.WithTimeout(ctx, 25*time.Minute)
			defer cancel()
			current, err := planner.ActionSnapshot(checkCtx, id, plan.Action, plan.RecoveryPointID)
			if err != nil {
				return err
			}
			if current.BaselineSHA256 != plan.BaselineSHA256 {
				return coordination.ErrBaseline
			}
			if err := json.Unmarshal(plan.Snapshot, &snapshot); err != nil {
				return err
			}
			if snapshot.BaselineSHA256 != current.BaselineSHA256 {
				return coordination.ErrPlan
			}
			if !plan.ExpiresAt.After(m.now().UTC()) {
				return coordination.ErrPlan
			}
			return nil
		}
		accept := func(op *Operation) error {
			// The plan's restore identity is known only after its locked validation.
			if request.Action == "restore" {
				op.RecoveryPointID = plan.RecoveryPointID
			}
			currentCounter := m.now().Unix() / 30
			if counter < currentCounter-1 || counter > currentCounter+1 {
				return authorization.ErrInvalid
			}
			bytes, err := json.Marshal(op)
			if err != nil {
				return err
			}
			a := coordination.Admission{SchemaVersion: 2, InstanceID: id, ClientRequestID: request.ClientRequestID, BusinessSHA256: request.BusinessSHA256(), OperationID: op.ID, Action: request.Action, PlanID: plan.PlanID, AcceptedEpoch: plan.ExpectedEpoch, Actor: plan.Actor, Scope: coordination.Scope(request.Action), Counter: counter, AcceptedAt: m.now().UTC(), RecoveryPointID: plan.RecoveryPointID, Operation: bytes}
			return m.coordinationStore().Accept(a)
		}
		kind := Kind(request.Action)
		if request.Action == "restore" {
			kind = KindRollback
		}
		run := func(ctx context.Context, op *Operation, save func() error) {
			switch request.Action {
			case "update":
				m.updateRunner(id, update.Options{AllowMaintenance: request.AllowMaintenance, ExpectedPlanSHA256: snapshot.UpdatePlanSHA256, PinnedTarget: snapshot.Target})(ctx, op, save)
			case "backup":
				m.backupRunner(id)(ctx, op, save)
			case "restore":
				op.RecoveryPointID = plan.RecoveryPointID
				m.rollbackRunner(id, plan.RecoveryPointID)(ctx, op, save)
			case "switch-back":
				m.switchBackRunner(id)(ctx, op, save)
			}
		}
		_, err := m.startAccepted(id, kind, "", run, validate, accept)
		// Once the authority is visible, errors in directory sync, projections or
		// cleanup must leave its request identity queryable and counter consumed.
		if found, lookupErr := lookup(); found {
			return nil
		} else if lookupErr != nil {
			return lookupErr
		}
		return err
	})
	return receipt, err
}

func admissionOperation(a coordination.Admission) (Operation, error) {
	var op Operation
	decoder := json.NewDecoder(bytes.NewReader(a.Operation))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&op); err != nil {
		return op, err
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return op, errors.New("invalid admission operation data")
	}
	kind := Kind(a.Action)
	if a.Action == "restore" {
		kind = KindRollback
	}
	if op.SchemaVersion != 1 || op.ID != a.OperationID || op.InstanceID != a.InstanceID || op.Kind != kind || op.StartedAt.IsZero() || len(op.Stages) > maximumOperationStages {
		return op, errors.New("admission operation identity is invalid")
	}
	switch op.Status {
	case StatusQueued, StatusRunning, StatusSucceeded, StatusFailed, StatusRolledBack, StatusRecoveryRequired:
	default:
		return op, errors.New("admission operation status is invalid")
	}
	return op, nil
}

// repairAdmissions is called with operation.lock held before any new action or
// reconciliation. Reading a receipt does not perform this repair or dispatch.
func (m *Manager) repairAdmissions(id string) error {
	store := m.coordinationStore()
	latest, err := store.Latest(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	current, err := headOperation(latest)
	if err != nil {
		return err
	}
	records, err := store.Admissions(id)
	if err != nil {
		return err
	}
	// Authority must be durable before any projection can survive independently.
	// Otherwise a lost admission link could masquerade as an old v1 operation.
	if err := store.SyncAuthority(id); err != nil {
		return err
	}
	directory := m.operationsDir(id)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return err
	}
	for _, a := range records {
		op, err := admissionOperation(a)
		if err != nil {
			return err
		}
		if err := replaceContents(filepath.Join(directory, op.ID+".json"), a.Operation); err != nil {
			return err
		}
	}
	if err := replaceContents(filepath.Join(directory, current.ID+".json"), latest.Operation); err != nil {
		return err
	}
	if err := replaceContents(m.currentPath(id), latest.Operation); err != nil {
		return err
	}
	return nil
}
func headOperation(head coordination.Head) (Operation, error) {
	var op Operation
	if err := json.Unmarshal(head.Operation, &op); err != nil {
		return op, err
	}
	action := string(op.Kind)
	switch op.Kind {
	case KindRollback:
		action = "restore"
	case KindUpdate, KindBackup, KindSwitchBack, KindVerify:
	default:
		return op, errors.New("invalid operation head kind")
	}
	return admissionOperation(coordination.Admission{InstanceID: head.InstanceID, OperationID: head.OperationID, Action: action, Operation: head.Operation})
}
