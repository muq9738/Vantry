// Package local exposes an api.Service over HTTP on a Unix domain socket.
package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	maxCommandBytes = 1 << 20
	maxErrorBytes   = 1 << 20
	maxEventBytes   = 4 << 20
)

type streamEnvelope struct {
	Event *api.Event `json:"event,omitempty"`
	Error *api.Error `json:"error,omitempty"`
}

func statusForCode(code api.ErrorCode) int {
	switch code {
	case api.InvalidCommand, api.VersionMismatch:
		return http.StatusBadRequest
	case api.NotFound:
		return http.StatusNotFound
	case api.Conflict, api.ResyncRequired:
		return http.StatusConflict
	case api.NotReady:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func asAPIError(err error) *api.Error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return &api.Error{Code: api.InternalError, Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	apiErr := asAPIError(err)
	writeJSON(w, statusForCode(apiErr.Code), apiErr)
}

func invalid(format string, args ...any) *api.Error {
	return &api.Error{Code: api.InvalidCommand, Message: fmt.Sprintf(format, args...)}
}
