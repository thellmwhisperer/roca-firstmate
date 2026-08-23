package scribe_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/scribe"
	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

func TestBackfillMirrorsEveryFabricatedMarkdownFamilyAndIsIncremental(t *testing.T) {
	home := copiedFabricatedHome(t)
	db := appliedDB(t)
	clock := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	ingester, err := scribe.New(context.Background(), db, scribe.Config{
		Home: home, HomeID: "northwind-harbor", Label: "Northwind Harbor",
		Kind: "primary", Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := ingester.Backfill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Scanned != 13 || first.Inserted != 13 || first.Unchanged != 0 {
		t.Fatalf("first backfill = %+v", first)
	}
	if first.WorkingSet != 5 || first.Archives != 3 || first.TaskState != 2 ||
		first.OperationalDocs != 1 || first.TaskArtifacts != 2 {
		t.Fatalf("family counts = %+v", first)
	}
	if first.Wakeups != first.Inserted {
		t.Fatalf("causal wakeups = %d, inserted = %d", first.Wakeups, first.Inserted)
	}
	assertCount(t, db, "wakeups", 13)

	second, err := ingester.Backfill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Scanned != 13 || second.Inserted != 0 || second.Unchanged != 13 || second.Wakeups != 0 {
		t.Fatalf("unchanged safety scan = %+v", second)
	}
	assertCount(t, db, "wakeups", 13)
}

func TestBackfillStopsBeforeFingerprintingWhenCanceled(t *testing.T) {
	home := copiedFabricatedHome(t)
	db := appliedDB(t)
	ingester, err := scribe.New(context.Background(), db, scribe.Config{
		Home: home, HomeID: "northwind-harbor", Kind: "primary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingester.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ingester.Backfill(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backfill error=%v", err)
	}
}

func TestFileEventCreatesVersionThenTriggerCreatesWakeup(t *testing.T) {
	home := copiedFabricatedHome(t)
	db := appliedDB(t)
	clock := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	ingester, err := scribe.New(context.Background(), db, scribe.Config{
		Home: home, HomeID: "northwind-harbor", Kind: "primary",
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingester.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(time.Minute)
	path := filepath.Join(home, "data", "captain.md")
	if err := os.WriteFile(path, []byte("# Fabricated captain\n\nPrefer the north lantern.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	event, err := ingester.IngestPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !event.Inserted || event.Version != 2 || event.Wakeups != 1 || event.Family != "working_set" {
		t.Fatalf("event = %+v", event)
	}

	var versions, current int
	if err := db.QueryRow(`SELECT COUNT(*), SUM(is_current) FROM working_set_versions
		WHERE home_id = 'northwind-harbor' AND relative_path = 'captain.md'`).Scan(&versions, &current); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || current != 1 {
		t.Fatalf("captain versions = %d, current rows = %d", versions, current)
	}
	var payload string
	if err := db.QueryRow(`SELECT payload FROM wakeups ORDER BY generation DESC LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"family":"working_set"`, `"relative_path":"captain.md"`, `"version":2`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("wakeup payload %s does not contain %s", payload, want)
		}
	}
}

func TestTotalIngestKeepsArbitraryAndNestedMarkdown(t *testing.T) {
	home := copiedFabricatedHome(t)
	data := filepath.Join(home, "data")
	if err := os.WriteFile(filepath.Join(data, "miscellany.md"), []byte("fabricated root note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(data, "lantern-1", "evidence")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "trace.md"), []byte("fabricated nested evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patternNested := filepath.Join(data, "task[1]", "evidence")
	if err := os.MkdirAll(patternNested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(patternNested, "trace.md"), []byte("fabricated pattern task evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	db := appliedDB(t)
	ingester, err := scribe.New(context.Background(), db, scribe.Config{
		Home: home, HomeID: "northwind-harbor", Kind: "primary",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ingester.Backfill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 16 || result.Inserted != 16 || result.OperationalDocs != 2 || result.TaskArtifacts != 4 {
		t.Fatalf("total ingest = %+v", result)
	}
	assertCount(t, db, "wakeups", 16)
}

func TestCursorStoresNoAbsoluteHomePath(t *testing.T) {
	home := copiedFabricatedHome(t)
	db := appliedDB(t)
	ingester, err := scribe.New(context.Background(), db, scribe.Config{
		Home: home, HomeID: "northwind-harbor", Kind: "primary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingester.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT path FROM ingest_file_state ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(path, home) || !strings.HasPrefix(path, "firstmate:northwind-harbor/data/") {
			t.Fatalf("cursor path leaked or lost its opaque identity: %q", path)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func appliedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "scribe.db")+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := schema.Apply(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func copiedFabricatedHome(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate fabricated fixture")
	}
	source := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "homes", "northwind-harbor")
	destination := filepath.Join(t.TempDir(), "northwind-harbor")
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return destination
}

func assertCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", table, got, want)
	}
}
