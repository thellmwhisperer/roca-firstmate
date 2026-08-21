package schema_test

import (
	"database/sql"
	"path/filepath"
	"slices"
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
	for _, name := range []string{"homes", "tasks", "ingest_file_state", "wakeups", "chart_cache"} {
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
			t.Fatalf("wakeups accepted destination %q; v1 allows machine and companion only", dest)
		}
	}
	mustExec(t, db, `INSERT INTO wakeups (
			destination, handled, generation, kind, payload, created_at
		) VALUES ('machine', 0, 1, 'ready', 'fabricated', '2026-03-14T09:00:00Z')`)
	mustExec(t, db, `INSERT INTO wakeups (
			destination, handled, generation, kind, payload, created_at
		) VALUES ('companion', 0, 1, 'decide', 'fabricated', '2026-03-14T09:00:00Z')`)
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

func TestShippedDatabaseMatchesSchemaSQL(t *testing.T) {
	root := repoRoot(t)
	shipped, err := sql.Open("sqlite", "file:"+filepath.Join(root, "firstmate.db")+"?mode=ro")
	if err != nil {
		t.Fatalf("open shipped firstmate.db: %v", err)
	}
	t.Cleanup(func() { shipped.Close() })

	applied := appliedDB(t)
	if diff := tableDiff(t, shipped, applied); diff != "" {
		t.Fatalf("shipped firstmate.db tables differ from schema.sql:%s", diff)
	}
	for _, table := range tableNames(t, applied) {
		want := columnNames(t, applied, table)
		got := columnNames(t, shipped, table)
		if !slices.Equal(want, got) {
			t.Fatalf("shipped %s columns %v, schema.sql has %v", table, got, want)
		}
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

func tableDiff(t *testing.T, a, b *sql.DB) string {
	t.Helper()
	left, right := tableNames(t, a), tableNames(t, b)
	if slices.Equal(left, right) {
		return ""
	}
	return "\n  shipped: " + join(left) + "\n  schema:  " + join(right)
}

func join(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ", "
		}
		out += value
	}
	return out
}
