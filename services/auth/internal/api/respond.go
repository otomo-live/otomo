// Response helpers shared by the handlers. WriteError defines the error envelope;
// these two cover the success and internal-failure sides of the same contract.

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON writes status and body as JSON. It mirrors WriteError: the header is set
// immediately, so a caller must not have written to w first.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// internalError logs the cause with the request ID and returns the opaque COM-5 500.
// The cause is for the log, never the client: an internal error message can name a
// query or a row, and none of that belongs in a response body. The request ID is the
// thread an operator follows from the client's report back to the log line.
func internalError(w http.ResponseWriter, r *http.Request, log *slog.Logger, operation string, cause error) {
	log.LogAttrs(r.Context(), slog.LevelError, "internal error",
		slog.String("operation", operation),
		slog.Any("error", cause),
		slog.String("request_id", RequestID(r.Context())),
	)
	WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
}
