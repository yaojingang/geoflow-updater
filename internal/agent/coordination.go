package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/operation"
)

type coordinatedOperations interface {
	Capabilities(string) (map[string]any, error)
	CreatePlan(context.Context, string, coordination.PlanRequest) (coordination.Plan, error)
	Plan(string, string) (coordination.Plan, error)
	Submit(context.Context, string, coordination.SubmitRequest, string, operation.AdmissionAuthorizer) (coordination.Receipt, error)
	Request(string, string) (coordination.Receipt, error)
	OperationReceipt(string, string) (coordination.Receipt, error)
}

func (server Server) coordinatedRequest(response http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/v2/instances/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "primary" {
		writeError(response, 404, "not_found")
		return
	}
	id := parts[0]
	if !server.authorized(request, id) {
		writeError(response, 401, "unauthorized")
		return
	}
	manager, ok := server.Operations.(coordinatedOperations)
	if !ok {
		writeError(response, 503, "coordination_unavailable")
		return
	}
	var value any
	var err error
	status := http.StatusOK
	missing := "not_found"
	switch {
	case len(parts) == 2 && parts[1] == "capabilities":
		if request.Method != http.MethodGet {
			writeError(response, 405, "method_not_allowed")
			return
		}
		value, err = manager.Capabilities(id)
	case len(parts) == 2 && parts[1] == "plans":
		if request.Method != http.MethodPost {
			writeError(response, 405, "method_not_allowed")
			return
		}
		var payload coordination.PlanRequest
		if !decodeCoordinated(response, request, &payload) || !payload.Valid() {
			writeError(response, 400, "invalid_request")
			return
		}
		value, err = manager.CreatePlan(request.Context(), id, payload)
		status = http.StatusCreated
	case len(parts) == 3 && parts[1] == "plans":
		if request.Method != http.MethodGet {
			writeError(response, 405, "method_not_allowed")
			return
		}
		value, err = manager.Plan(id, parts[2])
		missing = "plan_not_found"
	case len(parts) == 2 && parts[1] == "operations":
		if request.Method != http.MethodPost {
			writeError(response, 405, "method_not_allowed")
			return
		}
		var payload coordination.SubmitRequest
		if !decodeCoordinated(response, request, &payload) || !payload.Valid() {
			writeError(response, 400, "invalid_request")
			return
		}
		authorizer, _ := server.Authorization.(operation.AdmissionAuthorizer)
		value, err = manager.Submit(request.Context(), id, payload, request.Header.Get("X-GEOFlow-Updater-Authorization"), authorizer)
		status = http.StatusAccepted
		if errors.Is(err, authorization.ErrRateLimited) {
			server.retryAfter(response, id, authorization.Scope(coordination.Scope(payload.Action)))
		}
	case len(parts) == 3 && parts[1] == "requests":
		if request.Method != http.MethodGet {
			writeError(response, 405, "method_not_allowed")
			return
		}
		if !coordination.ValidRequestID(parts[2]) {
			writeError(response, 400, "invalid_request")
			return
		}
		value, err = manager.Request(id, parts[2])
		missing = "request_not_found"
	case len(parts) == 3 && parts[1] == "operations":
		if request.Method != http.MethodGet {
			writeError(response, 405, "method_not_allowed")
			return
		}
		if !coordination.ValidOperationID(parts[2]) {
			writeError(response, 400, "invalid_request")
			return
		}
		value, err = manager.OperationReceipt(id, parts[2])
		missing = "operation_not_found"
	default:
		writeError(response, 404, "not_found")
		return
	}
	switch {
	case err == nil:
		writeJSON(response, status, value)
	case errors.Is(err, os.ErrNotExist):
		writeError(response, 404, missing)
	case errors.Is(err, coordination.ErrConflict):
		writeError(response, 409, "request_conflict")
	case errors.Is(err, coordination.ErrBaseline):
		writeError(response, 409, "baseline_changed")
	case errors.Is(err, coordination.ErrEpoch):
		writeError(response, 409, "epoch_changed")
	case errors.Is(err, coordination.ErrPlan):
		writeError(response, 409, "plan_invalid")
	case errors.Is(err, coordination.ErrConfirmation):
		writeError(response, 409, "confirmation_required")
	case errors.Is(err, operation.ErrActive):
		writeError(response, 409, "operation_active")
	case errors.Is(err, authorization.ErrUnconfigured):
		writeError(response, 503, "mutation_authorization_unconfigured")
	case errors.Is(err, authorization.ErrInvalid):
		writeError(response, 403, "mutation_authorization_invalid")
	case errors.Is(err, authorization.ErrReplay):
		writeError(response, 403, "mutation_authorization_replayed")
	case errors.Is(err, authorization.ErrRateLimited):
		writeError(response, 429, "mutation_authorization_rate_limited")
	default:
		writeError(response, 503, "coordination_unavailable")
	}
}
func decodeCoordinated(response http.ResponseWriter, request *http.Request, payload any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(response, request.Body, 16384))
	if err != nil {
		return false
	}
	tokenReader := json.NewDecoder(bytes.NewReader(data))
	if !uniqueJSON(tokenReader) {
		return false
	}
	var trailing any
	if !errors.Is(tokenReader.Decode(&trailing), io.EOF) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(payload); err != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}
	if _, ok := payload.(*coordination.SubmitRequest); ok {
		for _, name := range []string{"allow_maintenance", "confirm_host_access"} {
			if string(fields[name]) != "true" && string(fields[name]) != "false" {
				return false
			}
		}
	}
	return true
}
func uniqueJSON(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !uniqueJSON(decoder) {
				return false
			}
		}
	case '[':
		for decoder.More() {
			if !uniqueJSON(decoder) {
				return false
			}
		}
	default:
		return false
	}
	closing, err := decoder.Token()
	return err == nil && ((delimiter == '{' && closing == json.Delim('}')) || (delimiter == '[' && closing == json.Delim(']')))
}
func (server Server) retryAfter(response http.ResponseWriter, id string, scope authorization.Scope) {
	seconds := 900
	if reader, ok := server.Authorization.(interface {
		RetryAfter(string, authorization.Scope) int
	}); ok {
		seconds = reader.RetryAfter(id, scope)
	}
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 86400 {
		seconds = 86400
	}
	response.Header().Set("Retry-After", strconv.Itoa(seconds))
}
