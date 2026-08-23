package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunWritesAnInstallableArchive(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "roca-firstmate")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho packaged\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	archive := filepath.Join(t.TempDir(), "plugin.tar.gz")
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err := run(repo, binary, out, "0.5.0", archive); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plugin.json", "firstmate.db", "roca-firstmate", "checksums.txt"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("package missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive: %v", err)
	}
}
