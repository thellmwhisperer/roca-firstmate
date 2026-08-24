package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releasePleaseWorkflow struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
		} `yaml:"push"`
		WorkflowDispatch *struct{} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Steps []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			If   string            `yaml:"if"`
			With map[string]string `yaml:"with"`
			Run  string            `yaml:"run"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

type releasePleaseConfig struct {
	ReleaseType             string `json:"release-type"`
	IncludeVInTag           bool   `json:"include-v-in-tag"`
	IncludeComponentInTag   bool   `json:"include-component-in-tag"`
	BumpMinorPreMajor       bool   `json:"bump-minor-pre-major"`
	BumpPatchForMinorPreMaj bool   `json:"bump-patch-for-minor-pre-major"`
	Packages                map[string]struct {
		PackageName string `json:"package-name"`
		ReleaseAs   string `json:"release-as"`
		ExtraFiles  []struct {
			Type     string `json:"type"`
			Path     string `json:"path"`
			JSONPath string `json:"jsonpath"`
		} `json:"extra-files"`
	} `json:"packages"`
}

func TestReleasePleaseRunsOnMainAndCreatesTheTag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release-please.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow releasePleaseWorkflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse release-please workflow: %v", err)
	}
	if !containsString(workflow.On.Push.Branches, "main") {
		t.Fatalf("release-please branches = %v, want main", workflow.On.Push.Branches)
	}
	if workflow.On.WorkflowDispatch != nil {
		t.Fatal("release-please must not expose workflow_dispatch")
	}
	if workflow.Permissions["contents"] != "write" || workflow.Permissions["pull-requests"] != "write" {
		t.Fatalf("release-please permissions = %v, want contents and pull-requests write", workflow.Permissions)
	}
	job, ok := workflow.Jobs["release-please"]
	if !ok {
		t.Fatal("release-please workflow has no release-please job")
	}

	var validate, action, checksums, automerge bool
	for _, step := range job.Steps {
		if step.Env["TOKEN"] == "${{ secrets.RELEASE_PLEASE_TOKEN }}" &&
			strings.Contains(step.Run, `if [ -z "$TOKEN" ]`) {
			validate = true
		}
		if strings.HasPrefix(step.Uses, "googleapis/release-please-action@") &&
			step.With["token"] == "${{ secrets.RELEASE_PLEASE_TOKEN }}" &&
			step.With["config-file"] == "release-please-config.json" &&
			step.With["manifest-file"] == ".release-please-manifest.json" {
			action = true
		}
		if step.If == "${{ steps.release.outputs.pr }}" &&
			hasCommand(shellCommands(t, step.Run), []string{"make", "sync-db"}) {
			checksums = true
		}
		if step.If == "${{ steps.release.outputs.pr }}" &&
			hasCommand(shellCommands(t, step.Run), []string{"gh", "pr", "merge"}) {
			automerge = true
			if !hasCommand(shellCommands(t, step.Run), []string{"gh", "pr", "merge", "--merge", "--auto"}) &&
				!mergeAutoFlags(shellCommands(t, step.Run)) {
				t.Fatal("release PR auto-merge must use gh pr merge --merge --auto")
			}
		}
	}
	if !validate {
		t.Fatal("release-please does not fail closed when RELEASE_PLEASE_TOKEN is missing")
	}
	if !action {
		t.Fatal("release-please does not invoke the pinned action with the repository token and manifest")
	}
	if !checksums {
		t.Fatal("release-please does not refresh plugin.json checksums on the release PR")
	}
	if !automerge {
		t.Fatal("release-please does not auto-merge the release PR it creates")
	}
}

func TestReleasePleaseOwnsThePluginVersion(t *testing.T) {
	root := repoRoot(t)
	var config releasePleaseConfig
	if err := json.Unmarshal(readRepoFile(t, filepath.Join(root, "release-please-config.json")), &config); err != nil {
		t.Fatalf("release-please config is not valid JSON: %v", err)
	}
	pkg, ok := config.Packages["."]
	if !ok {
		t.Fatal("release-please does not declare the repository root as its package")
	}
	if config.ReleaseType != "go" || !config.IncludeVInTag || config.IncludeComponentInTag ||
		!config.BumpMinorPreMajor || config.BumpPatchForMinorPreMaj ||
		pkg.PackageName != pluginName || pkg.ReleaseAs != "" {
		t.Fatalf("release config = %+v package = %+v; want go, v-prefix, root package %s, 0.x minor feats, no pinned release",
			config, pkg, pluginName)
	}
	if len(pkg.ExtraFiles) != 1 || pkg.ExtraFiles[0].Type != "json" ||
		pkg.ExtraFiles[0].Path != packageFilename || pkg.ExtraFiles[0].JSONPath != "$.version" {
		t.Fatalf("plugin version is not owned by release-please: extra-files = %#v", pkg.ExtraFiles)
	}

	var manifest map[string]string
	if err := json.Unmarshal(readRepoFile(t, filepath.Join(root, ".release-please-manifest.json")), &manifest); err != nil {
		t.Fatalf("release-please manifest is not valid JSON: %v", err)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(manifest["."]) {
		t.Fatalf("manifest baseline = %q, want a stable semver", manifest["."])
	}

	identity, err := decodeIdentity(readRepoFile(t, filepath.Join(root, packageFilename)))
	if err != nil {
		t.Fatal(err)
	}
	if identity.Version != manifest["."] {
		t.Fatalf("plugin.json version = %q, want manifest version %q", identity.Version, manifest["."])
	}
}

func TestExtraFilesVersionBumpInvalidatesSourceChecksumsUntilSync(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	raw := readRepoFile(t, filepath.Join(root, packageFilename))
	identity, err := decodeIdentity(raw)
	if err != nil {
		t.Fatal(err)
	}
	next := bumpPatchVersion(t, identity.Version)
	updated := strings.Replace(string(raw), `"version": "`+identity.Version+`"`, `"version": "`+next+`"`, 1)
	if updated == string(raw) {
		t.Fatal("extra-files jsonpath did not change plugin.json version")
	}
	if err := os.WriteFile(filepath.Join(dir, packageFilename), []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	newPluginSum := sha256Hex([]byte(updated))
	declared := checksumLine(t, readRepoFile(t, filepath.Join(root, checksumsFilename)), packageFilename)
	if declared == newPluginSum {
		t.Fatal("source checksums.txt already matches a bumped plugin.json")
	}

	cmd := exec.Command("shasum", "-a", "256", packageFilename)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shasum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, checksumsFilename), out, 0o600); err != nil {
		t.Fatal(err)
	}
	refreshed := checksumLine(t, out, packageFilename)
	if refreshed != newPluginSum {
		t.Fatalf("synced checksum = %s, want %s", refreshed, newPluginSum)
	}
	got, err := decodeIdentity([]byte(updated))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != next {
		t.Fatalf("bumped plugin.json version = %q, want %q", got.Version, next)
	}
}

func mergeAutoFlags(commands [][]string) bool {
	for _, command := range commands {
		if len(command) < 3 || command[0] != "gh" || command[1] != "pr" || command[2] != "merge" {
			continue
		}
		var merge, auto bool
		for _, arg := range command[3:] {
			switch arg {
			case "--merge":
				merge = true
			case "--auto":
				auto = true
			}
		}
		if merge && auto {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func readRepoFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func checksumLine(t *testing.T, raw []byte, name string) string {
	t.Helper()
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return strings.ToLower(fields[0])
		}
	}
	t.Fatalf("checksums do not declare %s", name)
	return ""
}

func bumpPatchVersion(t *testing.T, version string) string {
	t.Helper()
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("version %q is not major.minor.patch", version)
	}
	return parts[0] + "." + parts[1] + ".999"
}
