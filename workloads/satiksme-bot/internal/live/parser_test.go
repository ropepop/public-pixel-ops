package live

import (
	"strings"
	"testing"
	"time"
)

func TestParseDepartureRows(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Riga")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	raw := "stop,3012\ntram,1,b-a,68420,35119,Imanta\nbus,22,a-b,68542,,Lidosta\n"
	stopID, rows, err := Parse(strings.NewReader(raw), time.Date(2026, 3, 10, 18, 0, 0, 0, loc), loc)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if stopID != "3012" {
		t.Fatalf("expected stop 3012, got %q", stopID)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].LiveRowID != "35119" {
		t.Fatalf("expected upstream id, got %+v", rows[0])
	}
	if rows[1].BindingKey == "" || !strings.Contains(rows[1].BindingKey, "Lidosta") {
		t.Fatalf("expected fallback binding key, got %q", rows[1].BindingKey)
	}
}

func TestParseFiltersAlreadyDepartedRows(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Riga")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	now := time.Date(2026, 3, 10, 19, 4, 0, 0, loc)
	raw := "stop,3012\ntram,1,b-a,68420,35119,Imanta\nbus,22,a-b,68610,78648,Lidosta\n"

	_, rows, err := Parse(strings.NewReader(raw), now, loc)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 future row, got %d", len(rows))
	}
	if rows[0].RouteLabel != "22" {
		t.Fatalf("expected future route 22, got %+v", rows[0])
	}
}
