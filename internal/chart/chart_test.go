package chart_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/chart"
	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

var frozen = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

func TestEmptyDBCreatesChartAndSecondCallIsCached(t *testing.T) {
	db := appliedDB(t)
	first, err := chart.GetOrCreate(db, frozen)
	if err != nil {
		t.Fatalf("first get-or-create: %v", err)
	}
	if first.Status != "created" {
		t.Fatalf("status %q, want created", first.Status)
	}
	if first.GeneratedAt != frozen.Format(time.RFC3339) {
		t.Fatalf("generated_at %q", first.GeneratedAt)
	}
	if first.WakeupsUnhandled != 0 {
		t.Fatalf("unhandled wakeups %d", first.WakeupsUnhandled)
	}

	second, err := chart.GetOrCreate(db, frozen.Add(time.Hour))
	if err != nil {
		t.Fatalf("second get-or-create: %v", err)
	}
	if second.Status != "cached" {
		t.Fatalf("status %q, want cached when watermark still holds", second.Status)
	}
	if second.Watermark != first.Watermark {
		t.Fatalf("cached watermark %q, want %q", second.Watermark, first.Watermark)
	}
	if second.GeneratedAt != first.GeneratedAt {
		t.Fatalf("cached chart changed generated_at")
	}
}

func TestNewRowRegeneratesChart(t *testing.T) {
	db := appliedDB(t)
	first, err := chart.GetOrCreate(db, frozen)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`); err != nil {
		t.Fatalf("insert home: %v", err)
	}
	second, err := chart.GetOrCreate(db, frozen.Add(time.Minute))
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if second.Status != "regenerated" {
		t.Fatalf("status %q, want regenerated", second.Status)
	}
	if second.Watermark == first.Watermark {
		t.Fatal("watermark did not move after a new home row")
	}
	if len(second.Homes) != 1 || second.Homes[0].HomeID != "northwind-harbor" {
		t.Fatalf("homes %+v", second.Homes)
	}
}

func TestHandledWakeupRegeneratesChart(t *testing.T) {
	db := appliedDB(t)
	if _, err := db.Exec(`INSERT INTO wakeups (destination, generation, created_at)
		VALUES ('machine', 1, '2026-03-14T09:00:00Z')`); err != nil {
		t.Fatalf("insert wakeup: %v", err)
	}
	first, err := chart.GetOrCreate(db, frozen)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.WakeupsUnhandled != 1 {
		t.Fatalf("unhandled wakeups %d, want 1", first.WakeupsUnhandled)
	}
	if _, err := db.Exec(`UPDATE wakeups SET handled = 1 WHERE id = 1`); err != nil {
		t.Fatalf("handle wakeup: %v", err)
	}
	second, err := chart.GetOrCreate(db, frozen.Add(time.Minute))
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if second.Status != "regenerated" || second.WakeupsUnhandled != 0 {
		t.Fatalf("chart after handled wakeup: %+v", second)
	}
	if second.Watermark == first.Watermark {
		t.Fatal("watermark did not move after handling a wakeup")
	}
}

func TestMultiHomeRowsIncludeIdentityAndUseTotalOrder(t *testing.T) {
	db := appliedDB(t)
	if _, err := db.Exec(`INSERT INTO homes (home_id, label, kind, recorded_at) VALUES
		('z-harbor', 'Z Harbor', 'primary', '2026-03-14T09:00:00Z'),
		('a-harbor', 'A Harbor', 'secondmate', '2026-03-14T09:00:00Z');
		INSERT INTO working_set_versions
			(home_id, relative_path, document_kind, version, is_current, content, content_sha256, observed_at) VALUES
		('z-harbor', 'captain.md', 'captain', 1, 1, 'z', 'z-sha', '2026-03-14T09:00:00Z'),
		('a-harbor', 'captain.md', 'captain', 1, 1, 'a', 'a-sha', '2026-03-14T09:00:00Z');
		INSERT INTO tasks (home_id, task_id) VALUES
		('z-harbor', 'z-task'), ('a-harbor', 'a-task');
		INSERT INTO task_artifact_versions
			(home_id, task_id, relative_path, document_kind, version, is_current, content, content_sha256, observed_at) VALUES
		('z-harbor', 'z-task', 'z-task/brief.md', 'brief', 1, 1, 'z', 'z-sha', '2026-03-14T09:00:00Z'),
		('a-harbor', 'a-task', 'a-task/brief.md', 'brief', 1, 1, 'a', 'a-sha', '2026-03-14T09:00:00Z')`); err != nil {
		t.Fatalf("seed homes: %v", err)
	}
	result, err := chart.GetOrCreate(db, frozen)
	if err != nil {
		t.Fatalf("get-or-create: %v", err)
	}
	if len(result.WorkingSet) != 2 || result.WorkingSet[0].HomeID != "a-harbor" || result.WorkingSet[1].HomeID != "z-harbor" {
		t.Fatalf("working set order %+v", result.WorkingSet)
	}
	if len(result.Artifacts) != 2 || result.Artifacts[0].HomeID != "a-harbor" || result.Artifacts[1].HomeID != "z-harbor" {
		t.Fatalf("artifact order %+v", result.Artifacts)
	}
	toon := chart.RenderTOON(result)
	if !strings.Contains(toon, "working_set[2]{home_id,relative_path") {
		t.Fatalf("TOON omits document home identity:\n%s", toon)
	}
}

func TestTOONIsBoundedAndHasHelp(t *testing.T) {
	db := appliedDB(t)
	result, err := chart.GetOrCreate(db, frozen)
	if err != nil {
		t.Fatalf("get-or-create: %v", err)
	}
	text := chart.RenderTOON(result)
	if !strings.Contains(text, "status: created") {
		t.Fatalf("TOON missing status:\n%s", text)
	}
	if !strings.Contains(text, "help[") {
		t.Fatalf("TOON missing help[]:\n%s", text)
	}
	if !strings.Contains(text, "roca-firstmate chart --json") {
		t.Fatalf("help does not point at --json:\n%s", text)
	}
	if !strings.Contains(text, "roca exec") {
		t.Fatalf("help does not point at SQL:\n%s", text)
	}
	if strings.Contains(text, "content:") {
		t.Fatal("chart dumped document content; it must stay bounded identity rows")
	}
}

func TestJSONEnvelope(t *testing.T) {
	db := appliedDB(t)
	result, err := chart.GetOrCreate(db, frozen)
	if err != nil {
		t.Fatalf("get-or-create: %v", err)
	}
	raw, err := chart.RenderJSON(result)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"status", "watermark", "generated_at", "homes", "working_set",
		"archives", "task_state", "operational_docs", "artifacts",
		"wakeups_unhandled", "help",
	} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("json envelope missing %s", key)
		}
	}
}

func appliedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/chart.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := schema.Apply(db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}
