package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

type templateCatalog struct {
	Objects      []templateObject            `json:"objects"`
	Columns      map[string][]templateColumn `json:"columns"`
	PluginSchema templatePluginSchema        `json:"plugin_schema"`
	RowCounts    map[string]int              `json:"row_counts"`
}

type templateObject struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	TblName string `json:"tbl_name"`
}

type templateColumn struct {
	CID     int     `json:"cid"`
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	NotNull int     `json:"notnull"`
	Dflt    *string `json:"dflt"`
	PK      int     `json:"pk"`
}

type templatePluginSchema struct {
	PluginName    string `json:"plugin_name"`
	SchemaVersion int    `json:"schema_version"`
	IndexVersion  int    `json:"index_version"`
}

func TestOpenDatabaseCreatesTemplateSchemaIdempotently(t *testing.T) {
	want := loadTemplateCatalog(t)
	path := filepath.Join(t.TempDir(), "plugin", "firstmate.db")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fresh install path already exists: %v", err)
	}

	db, err := openDatabase(path)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	got := dumpTemplateCatalog(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if diff := catalogDiff(want, got); diff != "" {
		t.Fatalf("first-run database differs from the removed template:%s", diff)
	}

	again, err := openDatabase(path)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	repeat := dumpTemplateCatalog(t, again)
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if diff := catalogDiff(got, repeat); diff != "" {
		t.Fatalf("second run is not idempotent:%s", diff)
	}
}

func loadTemplateCatalog(t *testing.T) templateCatalog {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate the test file")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", "template-schema.json"))
	if err != nil {
		t.Fatalf("read template catalog: %v", err)
	}
	var catalog templateCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("parse template catalog: %v", err)
	}
	return catalog
}

func dumpTemplateCatalog(t *testing.T, db *sql.DB) templateCatalog {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("list schema objects: %v", err)
	}
	defer rows.Close()
	catalog := templateCatalog{
		Columns:   map[string][]templateColumn{},
		RowCounts: map[string]int{},
	}
	for rows.Next() {
		var object templateObject
		if err := rows.Scan(&object.Type, &object.Name, &object.TblName); err != nil {
			t.Fatalf("scan schema object: %v", err)
		}
		catalog.Objects = append(catalog.Objects, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("schema objects: %v", err)
	}
	for _, object := range catalog.Objects {
		if object.Type != "table" {
			continue
		}
		info, err := db.Query(`PRAGMA table_info(` + object.Name + `)`)
		if err != nil {
			t.Fatalf("pragma table_info(%s): %v", object.Name, err)
		}
		var columns []templateColumn
		for info.Next() {
			var column templateColumn
			if err := info.Scan(&column.CID, &column.Name, &column.Type, &column.NotNull, &column.Dflt, &column.PK); err != nil {
				info.Close()
				t.Fatalf("scan column for %s: %v", object.Name, err)
			}
			columns = append(columns, column)
		}
		if err := info.Err(); err != nil {
			info.Close()
			t.Fatalf("columns for %s: %v", object.Name, err)
		}
		info.Close()
		catalog.Columns[object.Name] = columns
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + object.Name).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", object.Name, err)
		}
		catalog.RowCounts[object.Name] = count
	}
	if err := db.QueryRow(`SELECT plugin_name, schema_version, index_version FROM plugin_schema WHERE singleton = 1`).
		Scan(&catalog.PluginSchema.PluginName, &catalog.PluginSchema.SchemaVersion, &catalog.PluginSchema.IndexVersion); err != nil {
		t.Fatalf("plugin_schema row: %v", err)
	}
	return catalog
}

func catalogDiff(want, got templateCatalog) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	return "\nwant:\n" + string(wantJSON) + "\ngot:\n" + string(gotJSON)
}
