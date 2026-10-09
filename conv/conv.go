// Package conv converts between the values sqlc reads from SQLite and their
// API representations: int64 row IDs and string IDs, sql.Null* columns and
// pointers, 0/1 integers and bools, and unix-second integers and times.
//
// Functions named Null… return a sql.Null… value for writing a column;
// functions named …Ptr return a pointer, nil for NULL, so it marshals as a
// JSON null.
package conv

import (
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// ParseID parses a string ID from the API (or URL path) into an int64 row
// ID. IDs are serialized as strings over the wire.
func ParseID(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// FormatID formats an int64 row ID as a string for the API.
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

// FormatIDs formats int64 row IDs as strings for the API.
func FormatIDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = FormatID(id)
	}
	return out
}

// StringPtr converts a nullable column to a pointer, nil when NULL.
func StringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// NullString converts an optional field to a nullable column.
func NullString(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

// Int64Ptr converts a nullable column to a pointer, nil when NULL.
func Int64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// NullInt64 converts an optional field to a nullable column.
func NullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// Float64Ptr converts a nullable column to a pointer, nil when NULL.
func Float64Ptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

// NullFloat64 converts an optional field to a nullable column.
func NullFloat64(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

// Bool reads a 0/1 integer column as a bool. SQLite has no boolean type.
func Bool(v int64) bool {
	return v != 0
}

// BoolInt converts a bool to a 0/1 integer for writing a column.
func BoolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Unix reads a unix-seconds integer column as a UTC time.
func Unix(v int64) time.Time {
	return time.Unix(v, 0).UTC()
}

// UnixPtr reads a nullable unix-seconds column as a UTC time, nil when
// NULL.
func UnixPtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := Unix(n.Int64)
	return &t
}

// NullUnix converts an optional time to a nullable unix-seconds column.
func NullUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}
