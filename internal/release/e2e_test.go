package release

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestScratchHomeInstallDispatchAndReleaseUpdate(t *testing.T) {
	if os.Getenv("ROCA_E2E") != "1" {
		t.Skip("set ROCA_E2E=1 to run scratch-home install and update against roca")
	}
	roca, err := exec.LookPath("roca")
	if err != nil {
		t.Fatal("ROCA_E2E=1 requires the roca binary on PATH")
	}
	realBin := buildFirstmate(t)

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ROCA_PREFIX", bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	writeScratchConfig(t, home)
	dbPath := filepath.Join(home, ".roca", "roca.db")
	runRoca(t, roca, home, dbPath, "init")

	source := filepath.Join(t.TempDir(), "plugin.tar.gz")
	n1 := packageRepo(t, fakeBinary(t, "release-n1"), "0.4.0")
	if err := Archive(n1, source); err != nil {
		t.Fatal(err)
	}

	install := runRoca(t, roca, home, dbPath, "plugin", "install", source, "--yes")
	pluginDir := filepath.Join(home, ".roca", "plugins", "roca-firstmate")
	if _, err := os.Stat(filepath.Join(pluginDir, "roca-firstmate")); err != nil {
		t.Fatalf("install did not place the executable: %v\n%s", err, install)
	}
	if _, err := os.Stat(filepath.Join(bin, "roca-firstmate")); err != nil {
		t.Fatalf("install did not install the PATH executable: %v", err)
	}

	marker := "custodial-marker-n1"
	stampDatabase(t, filepath.Join(pluginDir, "firstmate.db"), marker)

	n := packageRepo(t, fakeBinary(t, "release-n"), "0.5.0")
	if err := Archive(n, source); err != nil {
		t.Fatal(err)
	}
	runRoca(t, roca, home, dbPath, "plugin", "update", "roca-firstmate", "--yes")

	got, err := os.ReadFile(filepath.Join(pluginDir, "roca-firstmate"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "release-n") || strings.Contains(string(got), "release-n1") {
		t.Fatalf("update did not replace the executable: %q", got)
	}
	assertMarker(t, filepath.Join(pluginDir, "firstmate.db"), marker)

	pathGot, err := os.ReadFile(filepath.Join(bin, "roca-firstmate"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pathGot), "release-n") {
		t.Fatalf("PATH executable was not replaced: %q", pathGot)
	}

	realDir := t.TempDir()
	if err := Package(Options{RepoRoot: repoRoot(t), Binary: realBin, OutDir: realDir, Version: "0.5.0"}); err != nil {
		t.Fatal(err)
	}
	realArchive := filepath.Join(t.TempDir(), "real.tar.gz")
	if err := Archive(realDir, realArchive); err != nil {
		t.Fatal(err)
	}
	runRoca(t, roca, home, dbPath, "plugin", "uninstall", "roca-firstmate", "--yes")
	runRoca(t, roca, home, dbPath, "plugin", "install", realArchive, "--yes")

	help := exec.Command(filepath.Join(bin, "roca-firstmate"), "--help")
	help.Env = os.Environ()
	out, err := help.CombinedOutput()
	if err != nil {
		t.Fatalf("installed executable --help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "attach") {
		t.Fatalf("installed executable help:\n%s", out)
	}

	dispatch := exec.Command(roca, "firstmate", "--help")
	dispatch.Env = append(os.Environ(), "HOME="+home, "ROCA_PREFIX="+bin, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dispatched, err := dispatch.CombinedOutput()
	if err != nil {
		t.Fatalf("roca firstmate --help: %v\n%s", err, dispatched)
	}
	if !strings.Contains(string(dispatched), "attach") {
		t.Fatalf("roca firstmate did not dispatch:\n%s", dispatched)
	}

	fresh := filepath.Join(t.TempDir(), "fresh.db")
	chart := exec.Command(filepath.Join(bin, "roca-firstmate"), "chart", "--db", fresh, "--help")
	chart.Env = os.Environ()
	if out, err := chart.CombinedOutput(); err != nil {
		t.Fatalf("chart --help: %v\n%s", err, out)
	}
	create := exec.Command(filepath.Join(bin, "roca-firstmate"), "chart",
		"--db", fresh,
		"--home", filepath.Join(repoRoot(t), "testdata", "homes", "northwind-harbor"),
		"--home-id", "northwind-harbor")
	create.Env = os.Environ()
	created, err := create.CombinedOutput()
	if err != nil {
		t.Fatalf("first-use chart: %v\n%s", err, created)
	}
	if !strings.Contains(string(created), "status:") {
		t.Fatalf("first-use chart output:\n%s", created)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("first use did not create the database: %v", err)
	}

	t.Logf("e2e counts: payload_files=4 marker_preserved=1 executable_replaced=1 firstmate_dispatch=1 fresh_db=1")
}

func TestExistingCustodialInstallUpdatesFromPackagedRelease(t *testing.T) {
	if os.Getenv("ROCA_E2E") != "1" {
		t.Skip("set ROCA_E2E=1 to run the existing-install update transition against roca")
	}
	roca, err := exec.LookPath("roca")
	if err != nil {
		t.Fatal("ROCA_E2E=1 requires the roca binary on PATH")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ROCA_PREFIX", bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeScratchConfig(t, home)
	dbPath := filepath.Join(home, ".roca", "roca.db")
	runRoca(t, roca, home, dbPath, "init")

	previous := dataOnlyPackage(t, "0.4.0")
	source := filepath.Join(t.TempDir(), "plugin.tar.gz")
	if err := Archive(previous, source); err != nil {
		t.Fatal(err)
	}
	runRoca(t, roca, home, dbPath, "plugin", "install", source, "--yes")

	pluginDir := filepath.Join(home, ".roca", "plugins", "roca-firstmate")
	marker := "operator-custodial-row"
	stampDatabase(t, filepath.Join(pluginDir, "firstmate.db"), marker)

	next := packageRepo(t, fakeBinary(t, "next"), "0.5.0")
	if err := Archive(next, source); err != nil {
		t.Fatal(err)
	}
	runRoca(t, roca, home, dbPath, "plugin", "update", "roca-firstmate", "--yes")
	assertMarker(t, filepath.Join(pluginDir, "firstmate.db"), marker)
	got, err := os.ReadFile(filepath.Join(pluginDir, "roca-firstmate"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "next") {
		t.Fatalf("existing-install update left executable %q", got)
	}
	t.Logf("e2e counts: data_only_n1=1 custodial_preserved=1 executable_added=1")
}

func dataOnlyPackage(t *testing.T, version string) string {
	t.Helper()
	out := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	binary, err := json.Marshal("roca")
	if err != nil {
		t.Fatal(err)
	}
	encodedVersion, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	document["binary"] = binary
	document["version"] = encodedVersion
	manifest, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "plugin.json"), append(manifest, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeEmptyDatabase(filepath.Join(out, "firstmate.db")); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(out, []string{"firstmate.db", "plugin.json"}); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeScratchConfig(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".roca")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("[features]\nplugins = true\n")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runRoca(t *testing.T, roca, home, dbPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command(roca, append([]string{"--db-path", dbPath}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+home, "ROCA_PREFIX="+filepath.Join(home, "bin"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("roca %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func stampDatabase(t *testing.T, path, marker string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS custodial_probe (id INTEGER PRIMARY KEY, marker TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO custodial_probe (marker) VALUES (?)`, marker); err != nil {
		t.Fatal(err)
	}
}

func assertMarker(t *testing.T, path, marker string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT marker FROM custodial_probe ORDER BY id DESC LIMIT 1`).Scan(&got); err != nil {
		t.Fatalf("custodial database was not preserved: %v", err)
	}
	if got != marker {
		t.Fatalf("custodial marker = %q, want %q", got, marker)
	}
}

func buildFirstmate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "roca-firstmate")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/roca-firstmate")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return path
}

func TestBrokenSourceChecksumsAreRejectedByRoca(t *testing.T) {
	if os.Getenv("ROCA_E2E") != "1" {
		t.Skip("set ROCA_E2E=1 to reproduce the installer checksum mismatch")
	}
	roca, err := exec.LookPath("roca")
	if err != nil {
		t.Fatal("ROCA_E2E=1 requires the roca binary on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ROCA_PREFIX", filepath.Join(home, "bin"))
	writeScratchConfig(t, home)
	dbPath := filepath.Join(home, ".roca", "roca.db")
	runRoca(t, roca, home, dbPath, "init")

	broken := dataOnlyPackage(t, "0.4.0")
	sum, err := fileSHA256(filepath.Join(broken, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "checksums.txt"), []byte(sum+"  plugin.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(roca, "--db-path", dbPath, "plugin", "install", broken, "--yes")
	cmd.Env = append(os.Environ(), "HOME="+home, "ROCA_PREFIX="+filepath.Join(home, "bin"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("broken checksums installed:\n%s", out)
	}
	text := string(out)
	if !strings.Contains(text, "checksums.txt declares") || !strings.Contains(text, "plugin.json") || !strings.Contains(text, "firstmate.db") {
		t.Fatalf("broken source error did not reproduce the installer mismatch:\n%s", text)
	}

	var payload map[string]any
	if json.Unmarshal(out, &payload) == nil {
		t.Logf("broken install json=%v", payload)
	}
	t.Logf("e2e counts: broken_checksums_rejected=1")
}
