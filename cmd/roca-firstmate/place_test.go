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

func TestReplacePlacedExecutableWhenRenameCannotOverwrite(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "roca-firstmate")
	tmpName := filepath.Join(dir, "replacement")
	if err := os.WriteFile(dest, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpName, []byte("new executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	blocked := false
	rename := func(oldPath, newPath string) error {
		if !blocked && oldPath == tmpName && newPath == dest {
			blocked = true
			return os.ErrExist
		}
		return os.Rename(oldPath, newPath)
	}
	if err := replacePlacedExecutable(tmpName, dest, rename, os.Remove); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new executable" {
		t.Fatalf("placed content=%q", content)
	}
	if _, err := os.Stat(tmpName + ".previous"); !os.IsNotExist(err) {
		t.Fatalf("backup remains: %v", err)
	}
}
