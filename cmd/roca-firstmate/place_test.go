package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaceCopiesExecutableIntoPluginDirectory(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"place", "--dir", dir}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d stderr %s stdout %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "status: placed") {
		t.Fatalf("stdout %s", stdout.String())
	}
	dest := filepath.Join(dir, "roca-firstmate")
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("placed mode %s", info.Mode())
	}
}

func TestPlaceDefaultsToDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROCA_FIRSTMATE_DB", filepath.Join(dir, "firstmate.db"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"place"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d stderr %s stdout %s", code, stderr.String(), stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "roca-firstmate")); err != nil {
		t.Fatal(err)
	}
}
