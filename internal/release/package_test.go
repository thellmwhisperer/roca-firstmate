package release

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestPackageChecksumsCoverTheInstallerPayload(t *testing.T) {
	out := packageRepo(t, fakeBinary(t, "n1"), "")
	got := checksumNames(t, out)
	want := []string{"firstmate.db", "plugin.json", "roca-firstmate"}
	if !slices.Equal(got, want) {
		t.Fatalf("checksums.txt declares %v, want exactly %v", got, want)
	}
	for _, name := range want {
		sum, err := fileSHA256(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
		if checksums(t, out)[name] != sum {
			t.Fatalf("checksum mismatch for %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "checksums.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestPackageShipsARunnableExecutableAndEmptyCustodialDatabase(t *testing.T) {
	payload := []byte("#!/bin/sh\necho packaged-firstmate\n")
	out := packageRepo(t, fakeBinaryWith(t, payload), "")
	info, err := os.Stat(filepath.Join(out, "roca-firstmate"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("packaged executable mode %s", info.Mode())
	}
	got, err := os.ReadFile(filepath.Join(out, "roca-firstmate"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("packaged executable bytes differ from the built binary")
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(out, "firstmate.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	var schemaVersion, indexVersion int
	if err := db.QueryRow(`SELECT plugin_name, schema_version, index_version FROM plugin_schema WHERE singleton = 1`).
		Scan(&name, &schemaVersion, &indexVersion); err != nil {
		t.Fatalf("packaged firstmate.db is not a first-run schema: %v", err)
	}
	if name != "roca-firstmate" || schemaVersion < 1 {
		t.Fatalf("plugin_schema name=%q version=%d", name, schemaVersion)
	}
}

func TestPackageRewritesManifestVersionAndRequiresPluginBinary(t *testing.T) {
	out := packageRepo(t, fakeBinary(t, "n1"), "0.9.9")
	raw, err := os.ReadFile(filepath.Join(out, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Binary  string `json:"binary"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "roca-firstmate" || manifest.Binary != "roca-firstmate" || manifest.Version != "0.9.9" {
		t.Fatalf("packaged manifest %+v", manifest)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(`{
  "schema": 1,
  "name": "roca-firstmate",
  "version": "0.0.0",
  "binary": "roca",
  "databases": [{"name":"firstmate","path":"firstmate.db","alias":"plugin_roca_firstmate","attachment":"on-demand","custody":true,"retention":"keep"}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Package(Options{RepoRoot: root, Binary: fakeBinary(t, "x"), OutDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "roca-firstmate") {
		t.Fatalf("host-binary package error = %v", err)
	}
}

func TestArchiveContainsOnlyPackageRootPayload(t *testing.T) {
	out := packageRepo(t, fakeBinary(t, "n1"), "0.5.0")
	archive := filepath.Join(t.TempDir(), "roca-firstmate-v0.5.0-darwin-arm64.tar.gz")
	if err := Archive(out, archive); err != nil {
		t.Fatal(err)
	}
	extracted := t.TempDir()
	if err := extractArchive(t, archive, extracted); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(extracted)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("archive contains directory %s", entry.Name())
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	want := []string{"checksums.txt", "firstmate.db", "plugin.json", "roca-firstmate"}
	if !slices.Equal(names, want) {
		t.Fatalf("archive files %v, want %v", names, want)
	}
}

func TestBrokenChecksumsOmittingTheDatabaseMatchTheOperatorFailure(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	source := checksumNamesFrom(string(raw))
	if !slices.Equal(source, []string{"plugin.json"}) {
		t.Fatalf("source-tree checksums.txt declares %v, want [plugin.json]", source)
	}
	out := packageRepo(t, fakeBinary(t, "n1"), "")
	got := checksumNames(t, out)
	if slices.Equal(got, source) {
		t.Fatal("packaged checksums.txt still omits firstmate.db")
	}
	want := []string{"firstmate.db", "plugin.json", "roca-firstmate"}
	if !slices.Equal(got, want) {
		t.Fatalf("packaged checksums.txt declares %v, want exactly %v", got, want)
	}
}

func packageRepo(t *testing.T, binary, version string) string {
	t.Helper()
	out := t.TempDir()
	opts := Options{RepoRoot: repoRoot(t), Binary: binary, OutDir: out, Version: version}
	if err := Package(opts); err != nil {
		t.Fatalf("package: %v", err)
	}
	return out
}

func fakeBinary(t *testing.T, stamp string) string {
	t.Helper()
	return fakeBinaryWith(t, []byte("#!/bin/sh\necho "+stamp+"\n"))
}

func fakeBinaryWith(t *testing.T, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "roca-firstmate")
	if err := os.WriteFile(path, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func checksums(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("checksums.txt has an invalid line %q", line)
		}
		out[fields[1]] = strings.ToLower(fields[0])
	}
	return out
}

func checksumNames(t *testing.T, dir string) []string {
	t.Helper()
	names := make([]string, 0, len(checksums(t, dir)))
	for name := range checksums(t, dir) {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func checksumNamesFrom(raw string) []string {
	var names []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 {
			names = append(names, fields[1])
		}
	}
	slices.Sort(names)
	return names
}

func extractArchive(t *testing.T, archive, dest string) error {
	t.Helper()
	return extractTarGz(archive, dest)
}

func fileSHA256(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate the test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
