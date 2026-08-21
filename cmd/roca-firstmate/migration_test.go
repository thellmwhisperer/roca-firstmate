package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

func TestOpenDatabaseMigratesFabricatedV1BeforeUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fabricated-v1.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Apply(db); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TRIGGER working_set_version_wakeup`,
		`DROP TRIGGER archive_version_wakeup`,
		`DROP TRIGGER task_state_version_wakeup`,
		`DROP TRIGGER operational_doc_version_wakeup`,
		`DROP TRIGGER task_artifact_version_wakeup`,
		`DROP TRIGGER task_artifact_path_insert`,
		`DROP TRIGGER task_artifact_path_update`,
		`DROP TABLE ingest_file_state`,
		`ALTER TABLE task_artifact_versions RENAME TO task_artifact_versions_v2`,
		`CREATE TABLE task_artifact_versions (
			id INTEGER PRIMARY KEY,
			home_id TEXT NOT NULL REFERENCES homes(home_id),
			task_id TEXT NOT NULL,
			relative_path TEXT NOT NULL CHECK (
				instr(relative_path, '..') = 0
				AND relative_path GLOB (task_id || '/*')
				AND relative_path NOT GLOB (task_id || '/*/*')
			),
			document_kind TEXT NOT NULL,
			version INTEGER NOT NULL CHECK (version >= 1),
			is_current INTEGER NOT NULL CHECK (is_current IN (0, 1)),
			content TEXT NOT NULL,
			content_sha256 TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			source_mtime TEXT,
			UNIQUE (home_id, relative_path, version),
			FOREIGN KEY (home_id, task_id) REFERENCES tasks(home_id, task_id)
		)`,
		`DROP TABLE task_artifact_versions_v2`,
		`CREATE UNIQUE INDEX task_artifact_current
			ON task_artifact_versions(home_id, relative_path) WHERE is_current = 1`,
		`CREATE INDEX task_artifact_task ON task_artifact_versions(home_id, task_id)`,
		`UPDATE plugin_schema SET schema_version = 1 WHERE singleton = 1`,
		`INSERT INTO homes (home_id, label, kind, recorded_at)
			VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`,
		`INSERT INTO tasks (home_id, task_id) VALUES ('northwind-harbor', 'lantern-1')`,
		`INSERT INTO working_set_versions (
			home_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'captain.md', 'captain', 1, 1,
			'fabricated captain', 'aaa', '2026-03-14T09:00:00Z'
		)`,
		`INSERT INTO task_artifact_versions (
			home_id, task_id, relative_path, document_kind, version, is_current,
			content, content_sha256, observed_at
		) VALUES (
			'northwind-harbor', 'lantern-1', 'lantern-1/brief.md', 'brief', 1, 1,
			'fabricated brief', 'bbb', '2026-03-14T09:00:00Z'
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fabricate v1: %v\n%s", err, statement)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.QueryRow(`SELECT schema_version FROM plugin_schema WHERE singleton = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("schema version = %d, want 2", version)
	}
	for table, want := range map[string]int{
		"working_set_versions":   1,
		"task_artifact_versions": 1,
		"ingest_file_state":      0,
	} {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
	var wakeupTriggers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'trigger' AND name IN (
			'working_set_version_wakeup', 'archive_version_wakeup',
			'task_state_version_wakeup', 'operational_doc_version_wakeup',
			'task_artifact_version_wakeup'
		)`).Scan(&wakeupTriggers); err != nil {
		t.Fatal(err)
	}
	if wakeupTriggers != 5 {
		t.Fatalf("wakeup triggers = %d, want 5", wakeupTriggers)
	}
	if err := schema.Migrate(db); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO tasks (home_id, task_id) VALUES ('northwind-harbor', 'task[1]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_artifact_versions (
		home_id, task_id, relative_path, document_kind, version, is_current,
		content, content_sha256, observed_at
	) VALUES (
		'northwind-harbor', 'task[1]', 'task[1]/evidence/trace.md', 'trace', 1, 1,
		'fabricated nested trace', 'ccc', '2026-03-14T10:00:00Z'
	)`); err != nil {
		t.Fatalf("literal pattern task path: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO task_artifact_versions (
		home_id, task_id, relative_path, document_kind, version, is_current,
		content, content_sha256, observed_at
	) VALUES (
		'northwind-harbor', 'task[1]', 'task1/evidence/trace.md', 'trace', 1, 1,
		'fabricated mismatch', 'ddd', '2026-03-14T11:00:00Z'
	)`); err == nil {
		t.Fatal("migration accepted a non-literal task path")
	}
}
