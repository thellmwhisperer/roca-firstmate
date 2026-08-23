// Package schema owns the firstmate.db DDL applied by database-backed verbs.
//
// This package is shared by Scribe's versioned mirror and Nerve's queue state.
package schema

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
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
	return applyContext(context.Background(), db)
}

func applyContext(ctx context.Context, db *sql.DB) error {
	return withImmediateTransaction(ctx, db, func(conn *sql.Conn) error {
		return applySchema(ctx, conn)
	})
}

func applySchema(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, SQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

func withImmediateTransaction(ctx context.Context, db *sql.DB, operation func(*sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire schema connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin schema bootstrap: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	if err := operation(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}
	committed = true
	return nil
}

// Ensure applies the current schema to an empty database, then migrates.
func Ensure(db *sql.DB) error {
	return EnsureContext(context.Background(), db)
}

// EnsureContext applies the current schema to an empty database, then migrates.
func EnsureContext(ctx context.Context, db *sql.DB) error {
	err := withImmediateTransaction(ctx, db, func(conn *sql.Conn) error {
		var name string
		err := conn.QueryRowContext(ctx, `SELECT plugin_name FROM plugin_schema WHERE singleton = 1`).Scan(&name)
		if missingPluginSchema(err) {
			return applySchema(ctx, conn)
		}
		if err != nil {
			return fmt.Errorf("read plugin schema: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return MigrateContext(ctx, db)
}

func missingPluginSchema(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such table")
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
