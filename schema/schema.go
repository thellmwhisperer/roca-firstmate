// Package schema owns the firstmate.db DDL shipped by this plugin.
//
// Scribe imports La Roca's public parser, provenance, incrementality, and
// corpus-writer contracts. This package stays the schema seat its mirror uses.
package schema

import (
	"database/sql"
	_ "embed"
)

// SQL is the version 2 declaration. Apply replays it on an empty database.
//
//go:embed schema.sql
var SQL string

// Apply creates the five versioned inventory families and identity tables.
func Apply(db *sql.DB) error {
	_, err := db.Exec(SQL)
	return err
}
