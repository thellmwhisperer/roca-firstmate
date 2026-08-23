// Package schema owns the firstmate.db DDL shipped by this plugin.
//
// This package is shared by Scribe's versioned mirror and Nerve's queue state.
package schema

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

// SQL is the version 3 declaration. Apply replays it on an empty database.
//
//go:embed schema.sql
var SQL string

//go:embed migration_v1_to_v2.sql
var migrationV1ToV2SQL string

//go:embed migration_v2_to_v3.sql
var migrationV2ToV3SQL string

// Apply creates the five versioned inventory families and identity tables.
func Apply(db *sql.DB) error {
	_, err := db.Exec(SQL)
	return err
}

// Migrate upgrades an existing plugin-owned database before it is used.
func Migrate(db *sql.DB) error {
	return MigrateContext(context.Background(), db)
}

func MigrateContext(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()

	var name string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT plugin_name, schema_version FROM plugin_schema WHERE singleton = 1`).Scan(&name, &version); err != nil {
		return fmt.Errorf("read plugin schema: %w", err)
	}
	if name != "roca-firstmate" {
		return fmt.Errorf("database belongs to plugin %q", name)
	}
	for version < 3 {
		switch version {
		case 1:
			if _, err := tx.ExecContext(ctx, migrationV1ToV2SQL); err != nil {
				return fmt.Errorf("migrate schema v1 to v2: %w", err)
			}
			version = 2
		case 2:
			if _, err := tx.ExecContext(ctx, migrationV2ToV3SQL); err != nil {
				return fmt.Errorf("migrate schema v2 to v3: %w", err)
			}
			version = 3
		default:
			return fmt.Errorf("unsupported schema version %d", version)
		}
	}
	if version != 3 {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
