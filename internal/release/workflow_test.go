package release

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

type releaseWorkflow struct {
	On struct {
		Push struct {
			Tags []string `yaml:"tags"`
		} `yaml:"push"`
		WorkflowDispatch struct {
			Inputs map[string]workflowInput `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowInput struct {
	Required bool `yaml:"required"`
}

type workflowJob struct {
	Steps []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
	With map[string]string `yaml:"with"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
}

func TestReleaseWorkflowBuildsBothPlatformsAndPublishesAGitHubRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	if !slices.Contains(workflow.On.Push.Tags, "v*") {
		t.Fatalf("release workflow tags = %v, want v*", workflow.On.Push.Tags)
	}
	if input, ok := workflow.On.WorkflowDispatch.Inputs["tag"]; !ok || !input.Required {
		t.Fatal("release workflow has no workflow_dispatch tag input")
	}
	publish, ok := workflow.Jobs["publish"]
	if !ok {
		t.Fatal("release workflow has no publish job")
	}
	distStep, ok := stepWithCommand(t, publish.Steps, []string{"make", "dist"})
	if !ok || distStep.Name != "darwin-arm64 and linux-amd64 archives" {
		t.Fatalf("publish dist step = %q, want both release platforms", distStep.Name)
	}
	var commands [][]string
	for _, step := range publish.Steps {
		commands = append(commands, shellCommands(t, step.Run)...)
	}
	for _, want := range [][]string{
		{"make", "check"},
		{"make", "dist"},
		{"gh", "release", "create"},
		{"gh", "release", "upload"},
	} {
		if !hasCommand(commands, want) {
			t.Fatalf("publish job commands %v do not include %v", commands, want)
		}
	}
}

func TestReleaseWorkflowRejectsUnsynchronizedTagVersions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	step, ok := workflowStepNamed(workflow.Jobs["publish"].Steps, "The tag matches release-please versions")
	if !ok {
		t.Fatal("release workflow has no version synchronization gate")
	}
	dir := t.TempDir()
	writeVersions := func(plugin, manifest string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"version":"`+plugin+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".release-please-manifest.json"), []byte(`{".":"`+manifest+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeVersions("0.6.0", "0.6.0")
	if out, err := runWorkflowScript(step.Run, dir, map[string]string{"TAG": "v0.6.0"}); err != nil {
		t.Fatalf("release gate rejected synchronized versions: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		plugin   string
		manifest string
		tag      string
	}{
		{plugin: "0.6.0", manifest: "0.5.0", tag: "v0.6.0"},
		{plugin: "0.6.0", manifest: "0.6.0", tag: "v0.5.0"},
	} {
		writeVersions(tc.plugin, tc.manifest)
		if out, err := runWorkflowScript(step.Run, dir, map[string]string{"TAG": tc.tag}); err == nil {
			t.Fatalf("release gate accepted plugin=%s manifest=%s tag=%s:\n%s", tc.plugin, tc.manifest, tc.tag, out)
		}
	}
}

func stepWithCommand(t *testing.T, steps []workflowStep, want []string) (workflowStep, bool) {
	t.Helper()
	for _, step := range steps {
		if hasCommand(shellCommands(t, step.Run), want) {
			return step, true
		}
	}
	return workflowStep{}, false
}

func shellCommands(t *testing.T, script string) [][]string {
	t.Helper()
	if script == "" {
		return nil
	}
	program, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatalf("parse workflow shell step: %v", err)
	}
	var commands [][]string
	syntax.Walk(program, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		var words []string
		for _, arg := range call.Args {
			if len(arg.Parts) != 1 {
				break
			}
			literal, ok := arg.Parts[0].(*syntax.Lit)
			if !ok {
				break
			}
			words = append(words, literal.Value)
		}
		if len(words) > 0 {
			commands = append(commands, words)
		}
		return true
	})
	return commands
}

func hasCommand(commands [][]string, want []string) bool {
	for _, command := range commands {
		if len(command) >= len(want) && slices.Equal(command[:len(want)], want) {
			return true
		}
	}
	return false
}
