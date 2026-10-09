package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// RequestError writes a 400 with {"error": err.Error()}. It fits
// oapi-codegen's RequestErrorHandlerFunc (body can't be decoded) and
// ErrorHandlerFunc (bad path or query parameter), where err describes what
// the client sent wrong.
func RequestError(w http.ResponseWriter, r *http.Request, err error) {
	writeJSONError(w, http.StatusBadRequest, err.Error())
}

// ResponseError logs err via slog's default logger, with the request's
// method and path, and writes a 500 with a generic message so the error's
// details never reach the client. It fits oapi-codegen's
// ResponseErrorHandlerFunc, which gets the error a handler returned;
// the generated default sends that error's text to the client.
func ResponseError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "Request failed", "err", err, "method", r.Method, "path", r.URL.Path)
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
