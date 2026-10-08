package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestWriteJSONAndError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusNotFound, "farm not found")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"farm not found"}` {
		t.Errorf("body = %s", body)
	}
}

func TestDecodeJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"Home"}`))
	var v struct{ Name string }
	if err := DecodeJSON(r, &v); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if v.Name != "Home" {
		t.Errorf("Name = %q, want Home", v.Name)
	}

	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`not json`))
	if err := DecodeJSON(r, &v); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestIDs(t *testing.T) {
	ids, err := ParseIDList(" 1, 2,,3 ")
	if err != nil {
		t.Fatalf("ParseIDList: %v", err)
	}
	if !slices.Equal(ids, []int64{1, 2, 3}) {
		t.Errorf("ParseIDList = %v, want [1 2 3]", ids)
	}
	if got := FormatIDs(ids); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Errorf("FormatIDs = %v", got)
	}
	if _, err := ParseIDList("1,x"); err == nil {
		t.Error("expected error for non-numeric ID")
	}
}

func TestNulls(t *testing.T) {
	if NullFloatPtr(sql.NullFloat64{}) != nil {
		t.Error("NullFloatPtr(NULL) should be nil")
	}
	f := 1.5
	if n := FloatPtrToNull(&f); !n.Valid || n.Float64 != 1.5 {
		t.Errorf("FloatPtrToNull = %v", n)
	}
	if p := NullFloatPtr(FloatPtrToNull(&f)); p == nil || *p != 1.5 {
		t.Errorf("float round trip = %v", p)
	}

	if StringPtrToNull(nil).Valid {
		t.Error("StringPtrToNull(nil) should be invalid")
	}
	s := "x"
	if p := NullStringPtr(StringPtrToNull(&s)); p == nil || *p != "x" {
		t.Errorf("string round trip = %v", p)
	}
}
