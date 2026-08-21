// Package schema owns the firstmate.db DDL shipped by this plugin.
//
// This package stays the schema seat used by Scribe's versioned mirror.
package schema

import (
	"database/sql"
	_ "embed"
	"fmt"
)

// SQL is the version 2 declaration. Apply replays it on an empty database.
//
//go:embed schema.sql
var SQL string

//go:embed migration_v1_to_v2.sql
var migrationV1ToV2SQL string

// Apply creates the five versioned inventory families and identity tables.
func Apply(db *sql.DB) error {
	_, err := db.Exec(SQL)
	return err
}

// Migrate upgrades an existing plugin-owned database before it is used.
func Migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()

	var name string
	var version int
	if err := tx.QueryRow(`SELECT plugin_name, schema_version FROM plugin_schema WHERE singleton = 1`).Scan(&name, &version); err != nil {
		return fmt.Errorf("read plugin schema: %w", err)
	}
	if name != "roca-firstmate" {
		return fmt.Errorf("database belongs to plugin %q", name)
	}
	switch version {
	case 1:
		if _, err := tx.Exec(migrationV1ToV2SQL); err != nil {
			return fmt.Errorf("migrate schema v1 to v2: %w", err)
		}
	case 2:
	default:
		return fmt.Errorf("unsupported schema version %d", version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
