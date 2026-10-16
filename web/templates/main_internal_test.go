package templates

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the display time zone so expected strings don't depend on the machine.
func TestMain(m *testing.M) {
	Location = time.UTC
	os.Exit(m.Run())
}

func TestLocalUsesDisplayZone(t *testing.T) {
	defer func(l *time.Location) { Location = l }(Location)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	Location = la
	ts := time.Date(2026, 10, 16, 20, 15, 0, 0, time.UTC)
	if got := TxClock(ts); got != "13:15" {
		t.Fatalf("TxClock = %q, want local 13:15", got)
	}
}
