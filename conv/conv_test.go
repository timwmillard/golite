package conv

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

func TestIDs(t *testing.T) {
	if id, err := ParseID("42"); err != nil || id != 42 {
		t.Errorf("ParseID = %d, %v", id, err)
	}
	if FormatID(42) != "42" {
		t.Errorf("FormatID = %q", FormatID(42))
	}

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
	if StringPtr(sql.NullString{}) != nil || NullString(nil).Valid {
		t.Error("string NULL should map to nil and back")
	}
	s := "x"
	if p := StringPtr(NullString(&s)); p == nil || *p != "x" {
		t.Errorf("string round trip = %v", p)
	}

	if Int64Ptr(sql.NullInt64{}) != nil || NullInt64(nil).Valid {
		t.Error("int64 NULL should map to nil and back")
	}
	i := int64(7)
	if p := Int64Ptr(NullInt64(&i)); p == nil || *p != 7 {
		t.Errorf("int64 round trip = %v", p)
	}

	if Float64Ptr(sql.NullFloat64{}) != nil || NullFloat64(nil).Valid {
		t.Error("float64 NULL should map to nil and back")
	}
	f := 1.5
	if p := Float64Ptr(NullFloat64(&f)); p == nil || *p != 1.5 {
		t.Errorf("float64 round trip = %v", p)
	}
}

func TestBool(t *testing.T) {
	if Bool(0) || !Bool(1) || !Bool(2) {
		t.Error("Bool: 0 is false, anything else true")
	}
	if BoolInt(true) != 1 || BoolInt(false) != 0 {
		t.Error("BoolInt: want 1 and 0")
	}
}

func TestUnix(t *testing.T) {
	if got := Unix(0); !got.Equal(time.Unix(0, 0)) || got.Location() != time.UTC {
		t.Errorf("Unix(0) = %v, want epoch in UTC", got)
	}

	if UnixPtr(sql.NullInt64{}) != nil || NullUnix(nil).Valid {
		t.Error("unix NULL should map to nil and back")
	}
	now := time.Now().Truncate(time.Second)
	if p := UnixPtr(NullUnix(&now)); p == nil || !p.Equal(now) {
		t.Errorf("unix round trip = %v, want %v", p, now)
	}
}
