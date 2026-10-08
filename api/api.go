// Package api has helpers for JSON HTTP APIs: writing responses and
// errors, decoding request bodies, and converting between the database's
// int64 IDs and nullable columns and their JSON representations.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes {"error": message} with the given status.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

// DecodeJSON decodes the JSON request body into v and closes the body.
func DecodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ParseID parses a string ID from the JSON API (or URL path) into an int64
// row ID. IDs are serialized as strings over the wire.
func ParseID(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// FormatID formats an int64 row ID as a string for the JSON API.
func FormatID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// ParseIDList parses a comma-separated list of string IDs (e.g. "1,2,3")
// into int64 row IDs. Empty entries are skipped.
func ParseIDList(s string) ([]int64, error) {
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := ParseID(p)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// FormatIDs formats int64 row IDs as strings for the JSON API.
func FormatIDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = FormatID(id)
	}
	return out
}

// NullFloatPtr converts a nullable column to a pointer, nil when NULL, so it
// marshals as a JSON null.
func NullFloatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

// FloatPtrToNull converts an optional JSON field to a nullable column.
func FloatPtrToNull(f *float64) sql.NullFloat64 {
	if f == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *f, Valid: true}
}

// NullStringPtr converts a nullable column to a pointer, nil when NULL, so
// it marshals as a JSON null.
func NullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// StringPtrToNull converts an optional JSON field to a nullable column.
func StringPtrToNull(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}
