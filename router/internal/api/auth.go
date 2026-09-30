package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmesh/pkg/types"
)

// ExtractBearer returns the Bearer token from the Authorization header.
// Returns "" if missing or malformed.
func ExtractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

func unauthorised(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintln(w, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`)
}

func serviceUnavailable(w http.ResponseWriter, msg string) {
	b, _ := json.Marshal(msg) // includes quotes and escaping
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, `{"error":{"message":%s,"type":"service_unavailable"}}`+"\n", b)
}

// WorkerErrorChunk is the terminal chunk that tells a request's handler the
// worker failed it. A final error is the backend rejecting the request
// itself, and its reason is what the caller needs to fix it; anything else is
// the worker's own trouble, whose details (addresses, internals) are not the
// caller's business.
func WorkerErrorChunk(msg types.ErrorMsg) types.ChunkMsg {
	reason := "the worker could not complete the request"
	if msg.Final && msg.Message != "" {
		reason = msg.Message
	}
	return types.ChunkMsg{
		Type:         "chunk",
		RequestID:    msg.RequestID,
		Done:         true,
		FinishReason: "error",
		Error:        reason,
	}
}

// workerError reports a request the worker failed, with the reason it gave.
func workerError(w http.ResponseWriter, msg string) {
	b, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `{"error":{"message":%s,"type":"worker_error"}}`+"\n", b)
}

func internalError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintln(w, `{"error":{"message":"internal error","type":"server_error"}}`)
}
