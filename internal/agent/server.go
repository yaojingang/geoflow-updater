package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/operation"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
)

var (
	instanceIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	controlTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

type StatusProvider interface {
	Run(context.Context, string) doctor.Report
}

type OperationManager interface {
	StartUpdate(string) (operation.Operation, error)
	StartBackup(string) (operation.Operation, error)
	StartRollback(string, string) (operation.Operation, error)
	StartVerify(string) (operation.Operation, error)
	Current(string) (operation.Operation, error)
	RecoveryPoints(string) ([]recovery.Point, error)
}

type MutationAuthorizer interface {
	Authorize(string, authorization.Scope, string, func() error) error
}

type Server struct {
	StateDir      string
	Version       string
	Status        StatusProvider
	Operations    OperationManager
	Authorization MutationAuthorizer
}

func (server Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", server.health)
	mux.HandleFunc("/v1/instances/", server.instanceRequest)

	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("Content-Type", "application/json")
		mux.ServeHTTP(response, request)
	})
}

func (server Server) health(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"schema_version": 1,
		"status":         "ok",
		"version":        server.Version,
	})
}

func (server Server) instanceRequest(response http.ResponseWriter, request *http.Request) {
	instanceID, endpoint, ok := instanceEndpoint(request.URL.Path)
	if !ok {
		writeError(response, http.StatusNotFound, "not_found")
		return
	}
	if !server.authorized(request, instanceID) {
		writeError(response, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch endpoint {
	case "status":
		server.instanceStatus(response, request, instanceID)
	case "updates":
		if request.Method != http.MethodPost {
			writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if request.ContentLength != 0 {
			writeError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		server.startMutationOperation(response, request, instanceID, authorization.ScopeUpdate, func() (operation.Operation, error) {
			return server.Operations.StartUpdate(instanceID)
		})
	case "backups":
		if request.Method == http.MethodGet {
			server.recoveryPoints(response, instanceID)
			return
		}
		if request.Method != http.MethodPost {
			writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if request.ContentLength != 0 {
			writeError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		server.startMutationOperation(response, request, instanceID, authorization.ScopeBackup, func() (operation.Operation, error) {
			return server.Operations.StartBackup(instanceID)
		})
	case "rollbacks":
		server.startRollback(response, request, instanceID)
	case "verify":
		if request.Method != http.MethodPost {
			writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if request.ContentLength != 0 {
			writeError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		server.startOperation(response, request, func() (operation.Operation, error) {
			return server.Operations.StartVerify(instanceID)
		})
	case "operations/current":
		server.currentOperation(response, request, instanceID)
	default:
		writeError(response, http.StatusNotFound, "not_found")
	}
}

func (server Server) instanceStatus(response http.ResponseWriter, request *http.Request, instanceID string) {
	if request.Method != http.MethodGet {
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if server.Status == nil {
		writeError(response, http.StatusServiceUnavailable, "status_unavailable")
		return
	}

	writeJSON(response, http.StatusOK, instanceStatusResponse{
		Report:         server.Status.Run(request.Context(), instanceID),
		UpdaterVersion: server.Version,
	})
}

func (server Server) startOperation(response http.ResponseWriter, request *http.Request, start func() (operation.Operation, error)) {
	if request.Method != http.MethodPost {
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if server.Operations == nil {
		writeError(response, http.StatusServiceUnavailable, "operations_unavailable")
		return
	}
	started, err := start()
	if errors.Is(err, operation.ErrActive) {
		writeError(response, http.StatusConflict, "operation_active")
		return
	}
	if errors.Is(err, operation.ErrInvalidRecoveryPoint) {
		writeError(response, http.StatusBadRequest, "invalid_recovery_point")
		return
	}
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "operation_failed")
		return
	}
	writeJSON(response, http.StatusAccepted, started)
}

func (server Server) startRollback(response http.ResponseWriter, request *http.Request, instanceID string) {
	if request.Method != http.MethodPost {
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if server.Operations == nil {
		writeError(response, http.StatusServiceUnavailable, "operations_unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 4096)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload struct {
		RecoveryPointID string `json:"recovery_point_id"`
	}
	if err := decoder.Decode(&payload); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	points, err := server.Operations.RecoveryPoints(instanceID)
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "recovery_points_unavailable")
		return
	}
	rollbackPointID, ok := websiteRollbackPointID(points)
	if !ok {
		writeError(response, http.StatusConflict, "rollback_checkpoint_unavailable")
		return
	}
	if rollbackPointID != payload.RecoveryPointID {
		writeError(response, http.StatusBadRequest, "rollback_requires_latest_update_checkpoint")
		return
	}
	server.startMutationOperation(response, request, instanceID, authorization.ScopeRollback, func() (operation.Operation, error) {
		return server.Operations.StartRollback(instanceID, payload.RecoveryPointID)
	})
}

func websiteRollbackPointID(points []recovery.Point) (string, bool) {
	for _, point := range points {
		if point.IsUpdateCheckpoint() {
			return point.ID, true
		}
	}

	return "", false
}

func (server Server) startMutationOperation(
	response http.ResponseWriter,
	request *http.Request,
	instanceID string,
	scope authorization.Scope,
	start func() (operation.Operation, error),
) {
	if server.Operations == nil {
		writeError(response, http.StatusServiceUnavailable, "operations_unavailable")
		return
	}
	code := request.Header.Get("X-GEOFlow-Updater-Authorization")
	if code == "" {
		writeError(response, http.StatusForbidden, "mutation_authorization_required")
		return
	}
	if server.Authorization == nil {
		writeError(response, http.StatusServiceUnavailable, "mutation_authorization_unavailable")
		return
	}
	var started operation.Operation
	err := server.Authorization.Authorize(instanceID, scope, code, func() error {
		var startErr error
		started, startErr = start()

		return startErr
	})
	switch {
	case err == nil:
		writeJSON(response, http.StatusAccepted, started)
	case errors.Is(err, authorization.ErrUnconfigured):
		writeError(response, http.StatusServiceUnavailable, "mutation_authorization_unconfigured")
	case errors.Is(err, authorization.ErrReplay):
		writeError(response, http.StatusForbidden, "mutation_authorization_replayed")
	case errors.Is(err, authorization.ErrInvalid):
		writeError(response, http.StatusForbidden, "mutation_authorization_invalid")
	case errors.Is(err, authorization.ErrRateLimited):
		writeError(response, http.StatusTooManyRequests, "mutation_authorization_rate_limited")
	case errors.Is(err, operation.ErrActive):
		writeError(response, http.StatusConflict, "operation_active")
	case errors.Is(err, operation.ErrInvalidRecoveryPoint):
		writeError(response, http.StatusBadRequest, "invalid_recovery_point")
	default:
		writeError(response, http.StatusServiceUnavailable, "operation_failed")
	}
}

func (server Server) currentOperation(response http.ResponseWriter, request *http.Request, instanceID string) {
	if request.Method != http.MethodGet {
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if server.Operations == nil {
		writeError(response, http.StatusServiceUnavailable, "operations_unavailable")
		return
	}
	current, err := server.Operations.Current(instanceID)
	if errors.Is(err, os.ErrNotExist) {
		writeError(response, http.StatusNotFound, "operation_not_found")
		return
	}
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "operation_unavailable")
		return
	}
	writeJSON(response, http.StatusOK, current)
}

func (server Server) recoveryPoints(response http.ResponseWriter, instanceID string) {
	if server.Operations == nil {
		writeError(response, http.StatusServiceUnavailable, "operations_unavailable")
		return
	}
	points, err := server.Operations.RecoveryPoints(instanceID)
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "recovery_points_unavailable")
		return
	}
	summaries := make([]recoveryPointSummary, 0, len(points))
	for _, point := range points {
		summaries = append(summaries, recoveryPointSummary{
			SchemaVersion:   point.SchemaVersion,
			ID:              point.ID,
			InstanceID:      point.InstanceID,
			Reason:          point.Reason,
			CreatedAt:       point.CreatedAt,
			Version:         point.Version,
			ReleaseSequence: point.ReleaseSequence,
		})
	}
	writeJSON(response, http.StatusOK, map[string]any{"schema_version": 1, "recovery_points": summaries})
}

type recoveryPointSummary struct {
	SchemaVersion   int       `json:"schema_version"`
	ID              string    `json:"id"`
	InstanceID      string    `json:"instance_id"`
	Reason          string    `json:"reason"`
	CreatedAt       time.Time `json:"created_at"`
	Version         string    `json:"version"`
	ReleaseSequence uint64    `json:"release_sequence"`
}

type instanceStatusResponse struct {
	doctor.Report
	UpdaterVersion string `json:"updater_version"`
}

func statusInstanceID(path string) (string, bool) {
	instanceID, endpoint, ok := instanceEndpoint(path)
	if !ok || endpoint != "status" {
		return "", false
	}
	return instanceID, true
}

func instanceEndpoint(path string) (string, string, bool) {
	const prefix = "/v1/instances/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 || len(parts) > 3 || !instanceIDPattern.MatchString(parts[0]) {
		return "", "", false
	}
	endpoint := strings.Join(parts[1:], "/")
	if endpoint == "" {
		return "", "", false
	}
	return parts[0], endpoint, true
}

func (server Server) authorized(request *http.Request, instanceID string) bool {
	provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if provided == request.Header.Get("Authorization") || !controlTokenPattern.MatchString(provided) {
		return false
	}
	stateDir := server.StateDir
	if stateDir == "" {
		stateDir = "/var/lib/geoflow-updater"
	}
	tokenPath := filepath.Join(stateDir, "instances", instanceID, "control.token")
	info, err := os.Lstat(tokenPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 {
		return false
	}
	expected, err := os.ReadFile(tokenPath)
	if err != nil || !controlTokenPattern.MatchString(strings.TrimSpace(string(expected))) {
		return false
	}
	expectedDigest := sha256.Sum256([]byte(strings.TrimSpace(string(expected))))
	providedDigest := sha256.Sum256([]byte(provided))

	return subtle.ConstantTimeCompare(expectedDigest[:], providedDigest[:]) == 1
}

func ListenAndServe(ctx context.Context, socketPath string, handler http.Handler) error {
	lock, err := acquireServerLock(socketPath + ".lock")
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	if err := removeStaleSocket(socketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o660); err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	shutdownComplete := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
		close(shutdownComplete)
	}()

	err = httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownComplete
		return nil
	}

	return err
}

func acquireServerLock(path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("another updater agent is already active")
	}
	return lock, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to replace a non-socket runtime path")
	}

	return os.Remove(path)
}

func writeError(response http.ResponseWriter, status int, code string) {
	writeJSON(response, status, map[string]any{"error": code})
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}
