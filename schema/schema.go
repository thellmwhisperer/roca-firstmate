// Package schema owns the firstmate.db DDL shipped by this plugin.
//
// Later PRs import La Roca public packages (parsers, provenance,
// incrementality, corpus writer) after they land. This package stays the
// schema seat those writers apply; it does not ingest a firstmate home.
package schema

import (
	"database/sql"
	_ "embed"
)

// SQL is the version 1 declaration. Apply replays it on an empty database.
//
//go:embed schema.sql
var SQL string

// Apply creates the five versioned inventory families and identity tables.
func Apply(db *sql.DB) error {
	_, err := db.Exec(SQL)
	return err
}
