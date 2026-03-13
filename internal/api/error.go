package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorResponse{Error: code, Message: msg})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, "bad_request", msg)
}

func notFound(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusNotFound, "not_found", msg)
}

func conflict(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusConflict, "conflict", msg)
}

func serviceUnavailable(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusServiceUnavailable, "service_unavailable", msg)
}

func serverError(w http.ResponseWriter, detail string) {
	slog.Error("internal server error", "detail", detail)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func forbidden(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusForbidden, "forbidden", msg)
}
