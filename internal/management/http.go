// Package management contains protocol-independent management API primitives.
package management

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"

	"github.com/azrtydxb/Kuvryn-Ant/api"
)

type requestIDKey struct{}

func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := rand.Text()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}
func Error(w http.ResponseWriter, r *http.Request, status int, code api.ErrorCode, message string) {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	if id == "" {
		id = rand.Text()
		w.Header().Set("X-Request-ID", id)
	}
	JSON(w, status, api.Error{Code: code, Message: message, RequestId: id})
}
func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		// Readiness detail is administrator-only; it is reached only with a bearer token.
		if detail := statusDetail.Load(); detail != nil && r.Header.Get("Authorization") != "" {
			(*detail).ServeHTTP(w, r)
			return
		}
		JSON(w, http.StatusOK, api.ServiceStatus{Name: "kuvryn-ant", ApiVersion: "v1"})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "management route not found")
	})
}
