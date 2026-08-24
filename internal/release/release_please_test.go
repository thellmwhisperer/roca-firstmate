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
		Steps []workflowStep `yaml:"steps"`
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
	if workflow.Permissions["checks"] != "read" || workflow.Permissions["contents"] != "write" ||
		workflow.Permissions["pull-requests"] != "write" {
		t.Fatalf("release-please permissions = %v, want checks read plus contents and pull-requests write", workflow.Permissions)
	}
	job, ok := workflow.Jobs["release-please"]
	if !ok {
		t.Fatal("release-please workflow has no release-please job")
	}

	validate, ok := workflowStepNamed(job.Steps, "Validate RELEASE_PLEASE_TOKEN is set")
	if !ok || validate.Env["TOKEN"] != "${{ secrets.RELEASE_PLEASE_TOKEN }}" {
		t.Fatal("release-please has no token validation step")
	}
	assertTokenValidation(t, validate)

	var action, checksums bool
	for _, step := range job.Steps {
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
	}
	if !action {
		t.Fatal("release-please does not invoke the pinned action with the repository token and manifest")
	}
	if !checksums {
		t.Fatal("release-please does not refresh plugin.json checksums on the release PR")
	}
	gate, ok := workflowStepNamed(job.Steps, "Merge release PR after CI succeeds")
	if !ok || gate.If != "${{ steps.release.outputs.pr }}" {
		t.Fatal("release-please has no release PR CI gate")
	}
	assertReleaseGate(t, gate)
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
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(identity.Version) {
		t.Fatalf("plugin.json version = %q, want a stable semver", identity.Version)
	}
	if identity.Version != manifest["."] && !versionIsSingleBump(identity.Version, manifest["."]) {
		t.Fatalf("plugin.json version %q is not the published manifest %q or one pending release bump",
			identity.Version, manifest["."])
	}
}

func TestVersionIsSingleBump(t *testing.T) {
	for _, tc := range []struct {
		current  string
		baseline string
		want     bool
	}{
		{current: "0.6.0", baseline: "0.5.0", want: true},
		{current: "0.5.1", baseline: "0.5.0", want: true},
		{current: "1.0.0", baseline: "0.9.4", want: true},
		{current: "9.0.0", baseline: "0.4.0", want: false},
		{current: "0.6.2", baseline: "0.5.0", want: false},
		{current: "0.5.2", baseline: "0.5.0", want: false},
	} {
		if got := versionIsSingleBump(tc.current, tc.baseline); got != tc.want {
			t.Errorf("versionIsSingleBump(%q, %q) = %v, want %v", tc.current, tc.baseline, got, tc.want)
		}
	}
}

func versionIsSingleBump(current, baseline string) bool {
	cur, ok := parseSemver(current)
	if !ok {
		return false
	}
	base, ok := parseSemver(baseline)
	if !ok {
		return false
	}
	return cur[0] == base[0] && cur[1] == base[1] && cur[2] == base[2]+1 ||
		cur[0] == base[0] && cur[1] == base[1]+1 && cur[2] == 0 ||
		cur[0] == base[0]+1 && cur[1] == 0 && cur[2] == 0
}

func parseSemver(version string) ([3]int, bool) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, part := range parts {
		if part == "" {
			return [3]int{}, false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return [3]int{}, false
			}
			out[i] = out[i]*10 + int(r-'0')
		}
	}
	return out, true
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
	if err := os.WriteFile(filepath.Join(dir, checksumsFilename), readRepoFile(t, filepath.Join(root, checksumsFilename)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), readRepoFile(t, filepath.Join(root, "Makefile")), 0o600); err != nil {
		t.Fatal(err)
	}
	newPluginSum := sha256Hex([]byte(updated))
	declared := checksumLine(t, readRepoFile(t, filepath.Join(dir, checksumsFilename)), packageFilename)
	if declared == newPluginSum {
		t.Fatal("source checksums.txt already matches a bumped plugin.json")
	}

	cmd := exec.Command("make", "sync-db")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make sync-db: %v\n%s", err, out)
	}
	refreshed := checksumLine(t, readRepoFile(t, filepath.Join(dir, checksumsFilename)), packageFilename)
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

func workflowStepNamed(steps []workflowStep, name string) (workflowStep, bool) {
	for _, step := range steps {
		if step.Name == name {
			return step, true
		}
	}
	return workflowStep{}, false
}

func assertTokenValidation(t *testing.T, step workflowStep) {
	t.Helper()
	if out, err := runWorkflowScript(step.Run, repoRoot(t), map[string]string{"TOKEN": ""}); err == nil {
		t.Fatalf("token validation accepted an empty token:\n%s", out)
	}
	if out, err := runWorkflowScript(step.Run, repoRoot(t), map[string]string{"TOKEN": "configured"}); err != nil {
		t.Fatalf("token validation rejected a configured token: %v\n%s", err, out)
	}
}

func assertReleaseGate(t *testing.T, step workflowStep) {
	t.Helper()
	dir := t.TempDir()
	gh := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = "api" ]; then
  printf '%b\n' "$FAKE_CHECK_STATE"
elif [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' "$FAKE_HEAD_SHA"
elif [ "$1" = "pr" ] && [ "$2" = "merge" ]; then
  shift 2
  printf '%s\n' "$*" >"$FAKE_MERGE_LOG"
else
  exit 2
fi
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o700); err != nil {
		t.Fatal(err)
	}
	mergeLog := filepath.Join(dir, "merge.log")
	env := map[string]string{
		"PATH":             dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RELEASE_PR":       `{"number":42}`,
		"GH_REPO":          "example/roca-firstmate",
		"CHECKS_TOKEN":     "checks-token",
		"FAKE_HEAD_SHA":    "abc123",
		"FAKE_MERGE_LOG":   mergeLog,
		"FAKE_CHECK_STATE": "completed\\tfailure",
	}
	if out, err := runWorkflowScript(step.Run, repoRoot(t), env); err == nil {
		t.Fatalf("release gate merged after a failed check:\n%s", out)
	}
	if _, err := os.Stat(mergeLog); !os.IsNotExist(err) {
		t.Fatalf("release gate invoked merge after a failed check: %v", err)
	}

	env["FAKE_CHECK_STATE"] = "completed\\tsuccess"
	if out, err := runWorkflowScript(step.Run, repoRoot(t), env); err != nil {
		t.Fatalf("release gate rejected a successful check: %v\n%s", err, out)
	}
	merged := strings.TrimSpace(string(readRepoFile(t, mergeLog)))
	if merged != "--merge --match-head-commit abc123 42" {
		t.Fatalf("release merge arguments = %q, want checked head commit", merged)
	}
}

func runWorkflowScript(script, dir string, values map[string]string) ([]byte, error) {
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Dir = dir
	blocked := make(map[string]bool, len(values))
	for key := range values {
		blocked[key] = true
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !blocked[key] {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range values {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	return cmd.CombinedOutput()
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
