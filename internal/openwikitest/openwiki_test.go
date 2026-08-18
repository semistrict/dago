package openwikitest

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestMaintainerWikiStructureAndLinks(t *testing.T) {
	root := repositoryRoot(t)
	required := []string{
		"openwiki/INSTRUCTIONS.md",
		"openwiki/index.md",
		"openwiki/quickstart.md",
		"openwiki/architecture/index.md",
		"openwiki/architecture/overview.md",
		"openwiki/engineering/index.md",
		"openwiki/engineering/operations-and-testing.md",
		"openwiki/workflows/index.md",
		"openwiki/workflows/terminal-agent.md",
		"openwiki/workflows/evaluation-and-delivery.md",
	}
	for _, name := range required {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(data)
		if strings.TrimSpace(text) == "" {
			t.Fatalf("%s is empty", name)
		}
		if strings.Contains(text, "/Users/") || strings.Contains(text, `C:\\Users\\`) {
			t.Fatalf("%s contains a machine-specific path", name)
		}
	}
	index, err := os.ReadFile(filepath.Join(root, "openwiki/index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(index), "---\nokf_version: \"0.1\"\n---\n") {
		t.Fatalf("openwiki/index.md has invalid front matter:\n%s", index)
	}

	linkPattern := regexp.MustCompile(`\[[^]]+\]\(([^)]+)\)`)
	err = filepath.WalkDir(filepath.Join(root, "openwiki"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, match := range linkPattern.FindAllStringSubmatch(string(data), -1) {
			target := strings.SplitN(match[1], "#", 2)[0]
			if target == "" || strings.Contains(target, "://") {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
			if strings.HasSuffix(target, "/") {
				resolved = filepath.Join(resolved, "index.md")
			}
			if _, statErr := os.Stat(resolved); statErr != nil {
				t.Errorf("%s links to missing %s: %v", filepath.ToSlash(path), target, statErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenWikiWorkflowIsPinnedScopedAndReviewOnly(t *testing.T) {
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github/workflows/openwiki-update.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, required := range []string{
		"workflow_dispatch:",
		"schedule:",
		"permissions:\n  contents: read",
		"contents: write",
		"pull-requests: write",
		"pnpm dlx openwiki@0.2.1 code --update --print",
		"OPENAI_API_KEY: ${{ secrets.OPENWIKI_OPENAI_API_KEY }}",
		"add-paths: openwiki",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("workflow missing %q", required)
		}
	}
	if strings.Contains(workflow, "\n  pull_request:") || strings.Contains(workflow, "npm install") {
		t.Fatal("wiki updates must not run on pull requests or install through npm")
	}
	pinned := regexp.MustCompile(`^[ \t]*uses: [^@\s]+@[0-9a-f]{40}(?:[ \t]+#.*)?$`)
	for _, line := range strings.Split(workflow, "\n") {
		if strings.Contains(line, "uses:") && !pinned.MatchString(line) {
			t.Errorf("workflow action is not pinned to a full commit: %q", line)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
