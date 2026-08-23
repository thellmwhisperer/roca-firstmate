package schema_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

var familyTables = []string{
	"working_set_versions",
	"archive_versions",
	"task_state_versions",
	"operational_doc_versions",
	"task_artifact_versions",
}

func TestApplyCreatesFiveVersionedFamilyTables(t *testing.T) {
	db := appliedDB(t)
	got := tableNames(t, db)
	for _, name := range familyTables {
		if !slices.Contains(got, name) {
			t.Fatalf("schema is missing family table %s; have %v", name, got)
		}
	}
	for _, name := range []string{"homes", "tasks", "ingest_file_state", "wakeups", "seats", "chart_cache"} {
		if !slices.Contains(got, name) {
			t.Fatalf("schema is missing identity table %s; have %v", name, got)
		}
	}
}

func TestWakeupsRejectsUnknownDestination(t *testing.T) {
	db := appliedDB(t)
	for _, dest := range []string{"pager", "phone"} {
		_, err := db.Exec(`INSERT INTO wakeups (
				destination, handled, generation, kind, payload, created_at
			) VALUES (?, 0, 1, 'ready', '', '2026-03-14T09:00:00Z')`, dest)
		if err == nil {
			t.Fatalf("wakeups accepted destination %q; v1 allows captain, companion, and machine only", dest)
		}
	}
	mustExec(t, db, `INSERT INTO wakeups (
			destination, handled, generation, kind, payload, created_at
		) VALUES ('machine', 0, 1, 'ready', 'fabricated', '2026-03-14T09:00:00Z')`)
	mustExec(t, db, `INSERT INTO wakeups (
			destination, handled, generation, kind, payload, created_at
		) VALUES ('companion', 0, 1, 'decide', 'fabricated', '2026-03-14T09:00:00Z')`)
	mustExec(t, db, `INSERT INTO wakeups (
			destination, handled, generation, kind, payload, created_at
		) VALUES ('captain', 0, 2, 'ready', 'fabricated', '2026-03-14T09:00:00Z')`)
}

func TestHandledWakeupRequiresItsGeneration(t *testing.T) {
	db := appliedDB(t)
	mustExec(t, db, `INSERT INTO wakeups (
		destination, generation, kind, payload, created_at
	) VALUES ('machine', 4, 'ready', 'fabricated', '2026-03-14T09:00:00Z')`)
	if _, err := db.Exec(`UPDATE wakeups SET handled = 1 WHERE generation = 4`); err == nil {
		t.Fatal("wakeup was handled without recording its generation")
	}
	if _, err := db.Exec(`UPDATE wakeups SET handled = 1, handled_generation = 3 WHERE generation = 4`); err == nil {
		t.Fatal("wakeup accepted a mismatched handled generation")
	}
	mustExec(t, db, `UPDATE wakeups SET handled = 1, handled_generation = generation WHERE generation = 4`)
}

func TestWorkingSetRewriteIsANewVersionRow(t *testing.T) {
	db := appliedDB(t)
	mustExec(t, db, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`)
	mustExec(t, db, `INSERT INTO working_set_versions (
			home_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'captain.md', 'captain', 1, 0,
			'first draft of fabricated captain preferences',
			'aaa', '2026-03-14T09:00:00Z'
		)`)
	mustExec(t, db, `INSERT INTO working_set_versions (
			home_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'captain.md', 'captain', 2, 1,
			'second draft of fabricated captain preferences',
			'bbb', '2026-03-14T10:00:00Z'
		)`)

	var current string
	var version int
	row := db.QueryRow(`SELECT content, version FROM working_set_versions
		WHERE home_id = 'northwind-harbor' AND relative_path = 'captain.md' AND is_current = 1`)
	if err := row.Scan(&current, &version); err != nil {
		t.Fatalf("current working-set row: %v", err)
	}
	if version != 2 || current != "second draft of fabricated captain preferences" {
		t.Fatalf("current file is version %d %q, want the latest rewrite", version, current)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM working_set_versions
		WHERE home_id = 'northwind-harbor' AND relative_path = 'captain.md'`).Scan(&count); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if count != 2 {
		t.Fatalf("got %d rows for captain.md, want both rewrites kept", count)
	}
}

func TestOneCurrentRowPerWorkingSetPath(t *testing.T) {
	db := appliedDB(t)
	mustExec(t, db, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`)
	mustExec(t, db, `INSERT INTO working_set_versions (
			home_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'learnings.md', 'learnings', 1, 1,
			'fabricated learning', 'ccc', '2026-03-14T09:00:00Z'
		)`)
	_, err := db.Exec(`INSERT INTO working_set_versions (
			home_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'learnings.md', 'learnings', 2, 1,
			'later fabricated learning', 'ddd', '2026-03-14T11:00:00Z'
		)`)
	if err == nil {
		t.Fatal("two current rows for the same working-set path were accepted")
	}
}

func TestWorkingSetRejectsUnknownFilename(t *testing.T) {
	db := appliedDB(t)
	mustExec(t, db, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`)
	_, err := db.Exec(`INSERT INTO working_set_versions (
			home_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'notes.md', 'notes', 1, 1,
			'not a working-set file', 'eee', '2026-03-14T09:00:00Z'
		)`)
	if err == nil {
		t.Fatal("working_set_versions accepted a filename outside the closed five-file set")
	}
}

func TestTaskArtifactRequiresTaskRow(t *testing.T) {
	db := appliedDB(t)
	mustExec(t, db, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`)
	_, err := db.Exec(`INSERT INTO task_artifact_versions (
			home_id, task_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'lantern-1', 'lantern-1/brief.md', 'brief', 1, 1,
			'fabricated brief', 'fff', '2026-03-14T09:00:00Z'
		)`)
	if err == nil {
		t.Fatal("task_artifact_versions accepted a task_id with no tasks row")
	}

	mustExec(t, db, `INSERT INTO tasks (home_id, task_id) VALUES ('northwind-harbor', 'lantern-1')`)
	mustExec(t, db, `INSERT INTO task_artifact_versions (
			home_id, task_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'lantern-1', 'lantern-1/brief.md', 'brief', 1, 1,
			'fabricated brief', 'fff', '2026-03-14T09:00:00Z'
		)`)
}

func TestNoReservedProvenanceColumnName(t *testing.T) {
	db := appliedDB(t)
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		cols := columnNames(t, db, table)
		if slices.Contains(cols, "database") {
			t.Fatalf("table %s declares reserved column database", table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("tables: %v", err)
	}
}

func TestFabricatedHomeFitsTheFiveFamilyTables(t *testing.T) {
	db := appliedDB(t)
	mustExec(t, db, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`)
	mustExec(t, db, `INSERT INTO tasks (home_id, task_id) VALUES ('northwind-harbor', 'lantern-1')`)

	insert := func(table, path, kind, extraCols, extraVals string) {
		t.Helper()
		query := `INSERT INTO ` + table + ` (
			home_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at` + extraCols + `
		) VALUES (
			'northwind-harbor', ?, ?, 1, 1,
			'fabricated', 'abc', '2026-03-14T09:00:00Z'` + extraVals + `
		)`
		mustExec(t, db, query, path, kind)
	}

	insert("working_set_versions", "captain.md", "captain", "", "")
	insert("archive_versions", "memory-archive.md", "memory-archive", "", "")
	insert("task_state_versions", "backlog.md", "backlog", "", "")
	insert("operational_doc_versions", "2026-03-14-decision-lantern-berth.md", "decision", "", "")
	mustExec(t, db, `INSERT INTO task_artifact_versions (
			home_id, task_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'lantern-1', 'lantern-1/brief.md', 'brief', 1, 1,
			'fabricated', 'abc', '2026-03-14T09:00:00Z'
		)`)
}

func TestTelemetryTablesRemainFreeForLaterAdditiveSchema(t *testing.T) {
	db := appliedDB(t)
	got := tableNames(t, db)
	for _, name := range []string{"status_events", "task_meta", "wake_queue"} {
		if slices.Contains(got, name) {
			t.Fatalf("%s already exists; v1 must leave state/ telemetry free for an additive later schema", name)
		}
	}
}

func TestEnsureMatchesTemplateCatalog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "testdata", "template-schema.json"))
	if err != nil {
		t.Fatalf("read template catalog: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse template catalog: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/ensured.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := schema.Ensure(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	got := dumpCatalog(t, db)
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode template catalog: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode ensured catalog: %v", err)
	}
	var wantNorm, gotNorm any
	if err := json.Unmarshal(wantJSON, &wantNorm); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(gotJSON, &gotNorm); err != nil {
		t.Fatal(err)
	}
	if diff := catalogValueDiff(wantNorm, gotNorm); diff != "" {
		t.Fatalf("ensured database differs from the removed template:%s", diff)
	}
	if err := schema.Ensure(db); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}
	repeat := dumpCatalog(t, db)
	repeatJSON, err := json.Marshal(repeat)
	if err != nil {
		t.Fatalf("encode repeat catalog: %v", err)
	}
	if string(gotJSON) != string(repeatJSON) {
		t.Fatalf("second ensure changed the catalog\nfirst: %s\nsecond: %s", gotJSON, repeatJSON)
	}
}

func TestApplyRollsBackIncompleteBootstrap(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/rollback.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `CREATE TABLE chart_cache (sentinel INTEGER)`)
	if err := schema.Apply(db); err == nil {
		t.Fatal("schema bootstrap succeeded despite a conflicting final table")
	}
	if got := tableNames(t, db); !slices.Equal(got, []string{"chart_cache"}) {
		t.Fatalf("failed bootstrap left schema objects behind: %v", got)
	}
	mustExec(t, db, `DROP TABLE chart_cache`)
	if err := schema.Ensure(db); err != nil {
		t.Fatalf("retry ensure after rollback: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT schema_version FROM plugin_schema WHERE singleton = 1`).Scan(&version); err != nil {
		t.Fatalf("read plugin schema after retry: %v", err)
	}
	if version != 3 {
		t.Fatalf("schema version after retry = %d, want 3", version)
	}
}

func appliedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/applied.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := schema.Apply(db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, query)
	}
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_schema
		WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("tables: %v", err)
	}
	return names
}

func columnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var dflt any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns for %s: %v", table, err)
	}
	return names
}

func dumpCatalog(t *testing.T, db *sql.DB) map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("list schema objects: %v", err)
	}
	defer rows.Close()
	var objects []map[string]string
	var tables []string
	for rows.Next() {
		var objectType, name, tblName, definition string
		if err := rows.Scan(&objectType, &name, &tblName, &definition); err != nil {
			t.Fatalf("scan schema object: %v", err)
		}
		objects = append(objects, map[string]string{
			"type": objectType, "name": name, "tbl_name": tblName, "sql": normalizeSchemaSQL(definition),
		})
		if objectType == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("schema objects: %v", err)
	}
	columns := map[string]any{}
	counts := map[string]int{}
	for _, table := range tables {
		info, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatalf("pragma table_info(%s): %v", table, err)
		}
		var cols []map[string]any
		for info.Next() {
			var cid, notNull, pk int
			var name, kind string
			var dflt any
			if err := info.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
				info.Close()
				t.Fatalf("scan column for %s: %v", table, err)
			}
			cols = append(cols, map[string]any{
				"cid": cid, "name": name, "type": kind, "notnull": notNull, "dflt": dflt, "pk": pk,
			})
		}
		if err := info.Err(); err != nil {
			info.Close()
			t.Fatalf("columns for %s: %v", table, err)
		}
		info.Close()
		columns[table] = cols
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	var pluginName string
	var schemaVersion, indexVersion int
	if err := db.QueryRow(`SELECT plugin_name, schema_version, index_version FROM plugin_schema WHERE singleton = 1`).
		Scan(&pluginName, &schemaVersion, &indexVersion); err != nil {
		t.Fatalf("plugin_schema row: %v", err)
	}
	return map[string]any{
		"objects": objects,
		"columns": columns,
		"plugin_schema": map[string]any{
			"plugin_name":    pluginName,
			"schema_version": schemaVersion,
			"index_version":  indexVersion,
		},
		"row_counts": counts,
	}
}

func normalizeSchemaSQL(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

func catalogValueDiff(want, got any) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	return fmt.Sprintf("\nwant:\n%s\ngot:\n%s", wantJSON, gotJSON)
}
