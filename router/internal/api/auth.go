package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

func internalError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintln(w, `{"error":{"message":"internal error","type":"server_error"}}`)
}
