package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
)

var (
	instanceIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	controlTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

type StatusProvider interface {
	Run(context.Context, string) doctor.Report
}

type Server struct {
	StateDir string
	Version  string
	Status   StatusProvider
}

func (server Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", server.health)
	mux.HandleFunc("/v1/instances/", server.instanceStatus)

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

func (server Server) instanceStatus(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	instanceID, ok := statusInstanceID(request.URL.Path)
	if !ok || !server.authorized(request, instanceID) {
		writeError(response, http.StatusUnauthorized, "unauthorized")
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

type instanceStatusResponse struct {
	doctor.Report
	UpdaterVersion string `json:"updater_version"`
}

func statusInstanceID(path string) (string, bool) {
	const prefix = "/v1/instances/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/status") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/status")
	if !instanceIDPattern.MatchString(id) {
		return "", false
	}

	return id, true
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
