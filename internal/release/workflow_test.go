package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseWorkflowBuildsBothPlatformsAndPublishesAGitHubRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := parseWorkflow(string(raw))
	if !strings.Contains(workflow["on.push.tags"], "v*") {
		t.Fatalf("release workflow is not tag-driven: %q", workflow["on.push.tags"])
	}
	if workflow["on.workflow_dispatch.inputs.tag"] == "" {
		t.Fatal("release workflow has no workflow_dispatch tag input")
	}
	run := workflow["jobs.publish.steps.run"]
	for _, want := range []string{
		"make check",
		"make dist",
		"darwin-arm64",
		"linux-amd64",
		"gh release create",
		"gh release upload",
		"firstmate.db",
		"plugin.json",
		"roca-firstmate",
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("publish job does not %s", want)
		}
	}
}

func parseWorkflow(body string) map[string]string {
	out := map[string]string{}
	var run strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "tags:"):
			out["on.push.tags"] = trim
		case strings.Contains(line, "description:") && strings.Contains(strings.ToLower(line), "tag"):
			out["on.workflow_dispatch.inputs.tag"] = trim
		case strings.HasPrefix(line, "        run:") || (!strings.HasPrefix(line, "      -") && run.Len() > 0 && (strings.HasPrefix(line, "          ") || strings.HasPrefix(line, "        "))):
			run.WriteString(trim)
			run.WriteByte('\n')
		}
	}
	out["jobs.publish.steps.run"] = run.String()
	return out
}
