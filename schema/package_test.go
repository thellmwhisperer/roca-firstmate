package schema_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thellmwhisperer/roca-firstmate/schema"
)

// Hidden La Roca bookkeeping tables may exist in firstmate.db without a
// semantic declaration. Everything else must be described, in CREATE order.
var hiddenTables = map[string]bool{
	"plugin_schema": true,
}

type pluginManifest struct {
	Schema    int    `json:"schema"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Binary    string `json:"binary"`
	Databases []struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		Alias      string `json:"alias"`
		Attachment string `json:"attachment"`
		Custody    bool   `json:"custody"`
		Retention  string `json:"retention"`
	} `json:"databases"`
	Semantic struct {
		Databases []struct {
			Database    string   `json:"database"`
			Description string   `json:"description"`
			Questions   []string `json:"questions"`
			Tables      []struct {
				Name        string   `json:"name"`
				Description string   `json:"description"`
				Questions   []string `json:"questions"`
				Columns     []string `json:"columns"`
			} `json:"tables"`
		} `json:"databases"`
	} `json:"semantic"`
	Vector *struct {
		Databases []struct {
			Database string `json:"database"`
			Tables   []struct {
				Name        string   `json:"name"`
				IDColumn    string   `json:"id_column"`
				TextColumns []string `json:"text_columns"`
			} `json:"tables"`
		} `json:"databases"`
	} `json:"vector"`
	Verbs []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Capability  string `json:"capability"`
	} `json:"verbs"`
	Capabilities []struct {
		Name    string   `json:"name"`
		Command []string `json:"command"`
	} `json:"capabilities"`
}

func loadPluginManifest(t *testing.T) pluginManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest pluginManifest
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	return manifest
}

func TestPluginManifestDescribesTheShippedDatabase(t *testing.T) {
	manifest := loadPluginManifest(t)
	if manifest.Schema != 1 || manifest.Name != "roca-firstmate" || manifest.Binary != "roca-firstmate" {
		t.Fatalf("plugin identity is schema=%d name=%q binary=%q", manifest.Schema, manifest.Name, manifest.Binary)
	}
	if strings.TrimSpace(manifest.Version) == "" {
		t.Fatal("plugin.json has no version")
	}
	if len(manifest.Databases) != 1 {
		t.Fatalf("want one database declaration, got %d", len(manifest.Databases))
	}
	dbDecl := manifest.Databases[0]
	if dbDecl.Name != "firstmate" || dbDecl.Path != "firstmate.db" || dbDecl.Alias != "plugin_roca_firstmate" {
		t.Fatalf("database declaration %+v", dbDecl)
	}
	if dbDecl.Attachment != "on-demand" || !dbDecl.Custody || strings.TrimSpace(dbDecl.Retention) == "" {
		t.Fatalf("database policy %+v", dbDecl)
	}
	if len(manifest.Semantic.Databases) != 1 || manifest.Semantic.Databases[0].Database != "firstmate" {
		t.Fatal("semantic fragment must describe the firstmate database")
	}
	if len(manifest.Semantic.Databases[0].Questions) == 0 {
		t.Fatal("semantic fragment declares no database-level questions")
	}

	db := appliedDB(t)
	declared := map[string][]string{}
	for _, table := range manifest.Semantic.Databases[0].Tables {
		if table.Description == "" || len(table.Columns) == 0 {
			t.Fatalf("table %s needs a description and columns", table.Name)
		}
		if slices.Contains(table.Columns, "database") {
			t.Fatalf("table %s declares reserved column database", table.Name)
		}
		declared[table.Name] = table.Columns
	}
	for _, family := range familyTables {
		if _, ok := declared[family]; !ok {
			t.Fatalf("semantic fragment omits family table %s", family)
		}
	}

	for _, table := range tableNames(t, db) {
		if hiddenTables[table] {
			continue
		}
		want, ok := declared[table]
		if !ok {
			t.Fatalf("semantic fragment omits database table %s", table)
		}
		got := columnNames(t, db, table)
		if !slices.Equal(want, got) {
			t.Fatalf("semantic columns for %s are %v but the database has %v", table, want, got)
		}
	}
	for name := range declared {
		if hiddenTables[name] {
			continue
		}
		if !slices.Contains(tableNames(t, db), name) {
			t.Fatalf("semantic fragment describes missing table %s", name)
		}
	}
}

func TestPluginManifestDeclaresEmbeddingsOnlyVectorSurfaces(t *testing.T) {
	manifest := loadPluginManifest(t)
	if manifest.Vector == nil || len(manifest.Vector.Databases) != 1 {
		t.Fatal("plugin.json must declare a vector fragment for the firstmate database")
	}
	fragment := manifest.Vector.Databases[0]
	if fragment.Database != "firstmate" {
		t.Fatalf("vector database is %q, want firstmate", fragment.Database)
	}
	if len(fragment.Tables) != len(familyTables) {
		t.Fatalf("vector tables are %d, want the %d prose and task-text families", len(fragment.Tables), len(familyTables))
	}
	for i, table := range fragment.Tables {
		want := familyTables[i]
		if table.Name != want {
			t.Fatalf("vector table %d is %s, want %s", i, table.Name, want)
		}
		if table.IDColumn != "id" {
			t.Fatalf("vector table %s id_column is %q, want id", table.Name, table.IDColumn)
		}
		if !slices.Equal(table.TextColumns, []string{"content"}) {
			t.Fatalf("vector table %s text_columns are %v, want [content]", table.Name, table.TextColumns)
		}
	}
	for _, verb := range manifest.Verbs {
		if verb.Name == "ingest" || verb.Capability == "ingest" {
			t.Fatal("vector selectivity is embeddings-only; plugin.json must not declare ingest")
		}
	}
	for _, capability := range manifest.Capabilities {
		if capability.Name == "ingest" {
			t.Fatal("vector selectivity is embeddings-only; plugin.json must not declare ingest")
		}
	}
}

func TestChecksumsCoverExactlyTheInstallPayload(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "checksums.txt"))
	// Source-tree checksums.txt fingerprints plugin.json only. The installable
	// payload (plugin.json, firstmate.db, roca-firstmate) is generated by
	// `make package` / `make dist` and checked in internal/release.
	if err != nil {
		t.Fatalf("read checksums.txt: %v", err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("checksums.txt has an invalid line %q", line)
		}
		want[fields[1]] = strings.ToLower(fields[0])
	}
	required := []string{"plugin.json"}
	slices.Sort(required)
	var declared []string
	for name := range want {
		declared = append(declared, name)
	}
	slices.Sort(declared)
	if !slices.Equal(required, declared) {
		t.Fatalf("checksums.txt declares %v, want exactly %v", declared, required)
	}
	for name, expected := range want {
		sum, err := fileSHA256(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("hash %s: %v", name, err)
		}
		if sum != expected {
			t.Fatalf("checksum mismatch for %s: checksums.txt has %s, file is %s", name, expected, sum)
		}
	}
}

func TestRepositoryDoesNotCommitDatabaseBinaries(t *testing.T) {
	root := repoRoot(t)
	listed, err := exec.Command("git", "-C", root, "ls-files", "--", "*.db").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	if names := strings.TrimSpace(string(listed)); names != "" {
		t.Fatalf("committed database files:\n%s", names)
	}
	ignore := exec.Command("git", "-C", root, "check-ignore", "-q", "firstmate.db")
	if err := ignore.Run(); err != nil {
		t.Fatal("firstmate.db is not gitignored")
	}
}

func TestFabricatedHomeHasTheFiveFamiliesAndNoLivePaths(t *testing.T) {
	root := repoRoot(t)
	home := filepath.Join(root, "testdata", "homes", "northwind-harbor", "data")
	required := []string{
		"captain.md", "captain-shared.md", "learnings.md", "projects.md", "secondmates.md",
		"captain-archive.md", "memory-archive.md", "note-archive.md",
		"backlog.md", "done-archive.md",
		"2026-03-14-decision-lantern-berth.md",
		filepath.Join("lantern-1", "brief.md"),
		filepath.Join("lantern-1", "acceptance.md"),
	}
	for _, rel := range required {
		path := filepath.Join(home, rel)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("fabricated home is missing %s: %v", rel, err)
		}
	}

	leak := regexp.MustCompile(`(?i)(/Users/|/home/|/Volumes/|CrucialX9|\.treehouse/|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	err := filepath.WalkDir(filepath.Join(root, "testdata"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if leak.Find(body) != nil {
			t.Errorf("testdata %s contains a live-home marker", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan testdata: %v", err)
	}
}

func TestDresserSkillTeachesNerveV1Contract(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "skills", "dresser", "SKILL.md"))
	if err != nil {
		t.Fatalf("read dresser skill: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"## 1. Attach first",
		"fingerprint-sweeps Markdown",
		"prints the chart get-or-create plus the latest handoff",
		"Attaching is subscribing",
		"deterministic AXI",
		"roca-firstmate chart",
		"roca exec",
		"plugin_roca_firstmate",
		"### Native: milliseconds",
		"Claude Monitor",
		"Claude `asyncRewake`",
		"client.session.promptAsync",
		"never launch this recipe with `--pure`",
		"TypeScript extension",
		"`background-notify`",
		"Hermes: hooks are probable",
		"### Injection: seconds",
		"Codex CLI and Cursor CLI",
		"roca-firstmate follow --destination companion | fm-send",
		"### Passive: on open",
		"pending wakeup lines",
		"native in milliseconds, injection in seconds, and passive on open",
		"Tick keeps no in-memory state",
		"resident daemon subcommand may be added later",
		"conversation door",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dresser skill does not teach Nerve v1 contract text %q", want)
		}
	}
	for _, banned := range []string{"session-start", "Distiller", "rides.toml"} {
		if strings.Contains(text, banned) {
			t.Fatalf("dresser skill mentions %q; chart is on-demand AXI, not a hook or cron", banned)
		}
	}
}

func TestRepoHasNoSessionStartHooks(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"rides.toml",
		filepath.Join(".claude", "settings.json"),
		filepath.Join("hooks", "hooks.json"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Fatalf("found %s; this plugin must not run at session-start", rel)
		}
	}
}

func TestSchemaSQLEmbedIsTheRepoFile(t *testing.T) {
	root := repoRoot(t)
	onDisk, err := os.ReadFile(filepath.Join(root, "schema", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	if string(onDisk) != schema.SQL {
		t.Fatal("embedded schema.SQL does not match schema/schema.sql")
	}
}

func fileSHA256(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
