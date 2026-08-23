package watchlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendWritesRaiseLeaseAndSweepEvents(t *testing.T) {
	dir := t.TempDir()
	frozen := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)
	log := Open(dir, func() time.Time { return frozen })

	if err := log.Append(Event{Kind: KindRaise, HomeID: "northwind-harbor"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{Kind: KindLeaseAcquired, HomeID: "northwind-harbor", SeatID: "watch-northwind-harbor"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{
		Kind: KindSweep, HomeID: "northwind-harbor",
		Scanned: 4, Inserted: 1, Unchanged: 3, Wakeups: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{Kind: KindLeaseLost, HomeID: "northwind-harbor"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{Kind: KindCrashRetry, Attempt: 2, Err: "fabricated crash"}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "watch-2026-03-14.jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %d body %s", len(lines), body)
	}
	var kinds []string
	for _, line := range lines {
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("jsonl %q: %v", line, err)
		}
		if event.Timestamp != frozen {
			t.Fatalf("timestamp %v", event.Timestamp)
		}
		kinds = append(kinds, event.Kind)
	}
	want := []string{KindRaise, KindLeaseAcquired, KindSweep, KindLeaseLost, KindCrashRetry}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds %v want %v", kinds, want)
	}
}

func TestAppendNilLoggerIsNoop(t *testing.T) {
	var log *Logger
	if err := log.Append(Event{Kind: KindRaise}); err != nil {
		t.Fatalf("nil logger: %v", err)
	}
}
