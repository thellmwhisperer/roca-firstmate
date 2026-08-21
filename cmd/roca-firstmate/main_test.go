package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

func TestRunCreatesChartWithoutInitCeremony(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firstmate.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := schema.Apply(db); err != nil {
		t.Fatalf("apply: %v", err)
	}
	db.Close()
	home := filepath.Join("..", "..", "testdata", "homes", "northwind-harbor")

	var stdout, stderr bytes.Buffer
	code := run([]string{"chart", "--db", path, "--home", home, "--home-id", "northwind-harbor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: created") {
		t.Fatalf("stdout %s", stdout.String())
	}
}

func TestRunJSONAndUsageExitCodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firstmate.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := schema.Apply(db); err != nil {
		t.Fatalf("apply: %v", err)
	}
	db.Close()
	home := filepath.Join("..", "..", "testdata", "homes", "northwind-harbor")

	var stdout bytes.Buffer
	if code := run([]string{"chart", "--db", path, "--home", home, "--home-id", "northwind-harbor", "--json"}, &stdout, os.Stderr); code != 0 {
		t.Fatalf("json exit %d", code)
	}
	if !strings.Contains(stdout.String(), `"status"`) {
		t.Fatalf("json stdout %s", stdout.String())
	}

	var usage bytes.Buffer
	if code := run([]string{"--help"}, &usage, os.Stderr); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	var chartHelp bytes.Buffer
	if code := run([]string{"chart", "--help"}, os.Stdout, &chartHelp); code != 0 {
		t.Fatalf("chart help exit %d", code)
	}
	if !strings.Contains(chartHelp.String(), "Usage:") {
		t.Fatalf("chart help stderr %s", chartHelp.String())
	}
	if code := run([]string{"nope"}, &usage, os.Stderr); code != 2 {
		t.Fatalf("unknown command exit %d, want 2", code)
	}
	var missing bytes.Buffer
	if code := run([]string{"chart", "--db", filepath.Join(t.TempDir(), "absent.db"), "--home", home, "--home-id", "northwind-harbor"}, &missing, os.Stderr); code != 1 {
		t.Fatalf("missing db exit %d, want 1", code)
	}
}

func TestRunScribeBackfillsFabricatedHome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firstmate.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := schema.Apply(db); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	home := filepath.Join("..", "..", "testdata", "homes", "northwind-harbor")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"scribe", "--db", path, "--home", home,
		"--home-id", "northwind-harbor", "--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stderr %s stdout %s", code, stderr.String(), stdout.String())
	}
	for _, want := range []string{`"status":"mirrored"`, `"scanned":13`, `"inserted":13`, `"wakeups":13`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout %s does not contain %s", stdout.String(), want)
		}
	}
}
