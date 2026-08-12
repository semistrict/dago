package dadev

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveGraphsAndGenerateWrapper(t *testing.T) {
	directory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	graphs, err := resolveGraphs(context.Background(), projectConfig{Graphs: map[string]graphSpec{
		"agent": {Path: "./examples/studio:NewAgent", Description: "Example"},
	}}, directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(graphs) != 1 || graphs[0].ImportPath != "github.com/semistrict/dago/examples/studio" || graphs[0].Symbol != "NewAgent" {
		t.Fatalf("graphs = %#v", graphs)
	}
	output := filepath.Join(t.TempDir(), "main.go")
	if err := generateMain(graphs, output); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"daserver.AdaptFactory(graph0.NewAgent)", `ID: "agent"`, "daserver.ListenAndServe"} {
		if !strings.Contains(string(source), fragment) {
			t.Fatalf("generated source does not contain %q:\n%s", fragment, source)
		}
	}
}

func TestGraphFactoryMustBeIdentifier(t *testing.T) {
	_, err := resolveGraphs(context.Background(), projectConfig{Graphs: map[string]graphSpec{
		"agent": {Path: ".:not-valid"},
	}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a Go identifier") {
		t.Fatalf("err = %v", err)
	}
}

func TestGoModuleRootIncludesDependenciesAboveConfig(t *testing.T) {
	directory, err := filepath.Abs(filepath.Join("..", "..", "examples", "studio"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := goModuleRoot(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(filepath.Join("..", ".."))
	if root != want {
		t.Fatalf("module root = %q, want %q", root, want)
	}
}
