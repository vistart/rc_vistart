package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// errorResponse is the body of every non-2xx JSON response.
type errorResponse struct {
	// Error is a stable, machine-readable code such as "invalid_request".
	Error string `json:"error"`
	// Message is a human-readable explanation.
	Message string `json:"message"`
}

// writeJSON writes body as an indented JSON response. A nil body writes only
// the status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		// The status line is already on the wire, so the only thing left to
		// do is report the truncated response.
		slog.Default().Error("write json response failed", slog.String("error", err.Error()))
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}

// parseOptionalDuration parses a user-supplied duration. An empty string
// yields zero, meaning "use the service default".
func parseOptionalDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", d)
	}
	return d, nil
}

func bytesTrimSpace(b []byte) []byte {
	return bytes.TrimSpace(b)
}
