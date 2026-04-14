package api

import (
	"net/http"
	"strings"
)

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

func unauthorised(w http.ResponseWriter) {
	http.Error(w, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`, http.StatusUnauthorized)
}

func serviceUnavailable(w http.ResponseWriter, msg string) {
	http.Error(w, `{"error":{"message":"`+msg+`","type":"service_unavailable"}}`, http.StatusServiceUnavailable)
}

func internalError(w http.ResponseWriter) {
	http.Error(w, `{"error":{"message":"internal error","type":"server_error"}}`, http.StatusInternalServerError)
}
