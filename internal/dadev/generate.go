package dadev

import (
	"bytes"
	"context"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

type resolvedGraph struct {
	ID, Description, ImportPath, Symbol, Alias string
}

func resolveGraphs(ctx context.Context, config projectConfig, directory string) ([]resolvedGraph, error) {
	ids := make([]string, 0, len(config.Graphs))
	for id := range config.Graphs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	aliases := map[string]string{}
	result := make([]resolvedGraph, 0, len(ids))
	for _, id := range ids {
		spec := config.Graphs[id]
		separator := strings.LastIndex(spec.Path, ":")
		if separator <= 0 || separator == len(spec.Path)-1 {
			return nil, fmt.Errorf("graph %q must use path:Factory", id)
		}
		path, symbol := spec.Path[:separator], spec.Path[separator+1:]
		if !goIdentifier(symbol) {
			return nil, fmt.Errorf("graph %q factory %q is not a Go identifier", id, symbol)
		}
		importPath, err := resolveImport(ctx, directory, path)
		if err != nil {
			return nil, fmt.Errorf("graph %q: %w", id, err)
		}
		alias, exists := aliases[importPath]
		if !exists {
			alias = fmt.Sprintf("graph%d", len(aliases))
			aliases[importPath] = alias
		}
		result = append(result, resolvedGraph{ID: id, Description: spec.Description, ImportPath: importPath, Symbol: symbol, Alias: alias})
	}
	return result, nil
}

func resolveImport(ctx context.Context, directory, path string) (string, error) {
	if !strings.HasPrefix(path, ".") && !filepath.IsAbs(path) && !strings.HasSuffix(path, ".go") {
		return path, nil
	}
	target := path
	if strings.HasSuffix(target, ".go") {
		target = filepath.Dir(target)
	}
	if filepath.IsAbs(target) {
		relative, err := filepath.Rel(directory, target)
		if err != nil || strings.HasPrefix(relative, "..") {
			return "", fmt.Errorf("graph path must be inside the config module")
		}
		target = relative
	}
	target = filepath.ToSlash(target)
	if target == "." {
		target = "."
	} else if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") {
		target = "./" + target
	}
	command := exec.CommandContext(ctx, "go", "list", "-f", "{{.ImportPath}}", target)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve package %q: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func goIdentifier(value string) bool {
	for index, character := range value {
		if !(character == '_' || unicode.IsLetter(character) || index > 0 && unicode.IsDigit(character)) {
			return false
		}
	}
	return value != ""
}

func generateMain(graphs []resolvedGraph, output string) error {
	imports := map[string]string{}
	for _, graph := range graphs {
		imports[graph.ImportPath] = graph.Alias
	}
	paths := make([]string, 0, len(imports))
	for path := range imports {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var source bytes.Buffer
	source.WriteString("package main\n\nimport (\n\"context\"\n\"log\"\n\"os\"\n\"os/signal\"\n\"path/filepath\"\n\"strconv\"\n\"syscall\"\n")
	source.WriteString("\"github.com/semistrict/dago/dacheckpoint/sqlite\"\n")
	source.WriteString("\"github.com/semistrict/dago/daserver\"\n")
	source.WriteString("storesqlite \"github.com/semistrict/dago/dastore/sqlite\"\n")
	for _, path := range paths {
		fmt.Fprintf(&source, "%s %q\n", imports[path], path)
	}
	source.WriteString(")\n\nfunc main() {\n")
	source.WriteString("ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)\ndefer stop()\n")
	source.WriteString("stateDir := os.Getenv(\"DAGO_DEV_STATE_DIR\")\nif err := os.MkdirAll(stateDir, 0700); err != nil { log.Fatal(err) }\n")
	source.WriteString("saver, err := sqlite.Open(filepath.Join(stateDir, \"checkpoints.sqlite\"))\nif err != nil { log.Fatal(err) }\ndefer saver.Close()\n")
	source.WriteString("store, err := storesqlite.Open(filepath.Join(stateDir, \"store.sqlite\"))\nif err != nil { log.Fatal(err) }\ndefer store.Close()\n")
	source.WriteString("workers, err := strconv.Atoi(os.Getenv(\"DAGO_DEV_WORKERS\"))\nif err != nil { log.Fatal(err) }\n")
	source.WriteString("graphs := []daserver.GraphRegistration{\n")
	for _, graph := range graphs {
		fmt.Fprintf(&source, "{ID: %q, Description: %q, Factory: daserver.AdaptFactory(%s.%s)},\n", graph.ID, graph.Description, graph.Alias, graph.Symbol)
	}
	source.WriteString("}\n")
	source.WriteString("err = daserver.ListenAndServe(ctx, os.Getenv(\"DAGO_DEV_ADDRESS\"), daserver.Options{Graphs: graphs, Saver: saver, Store: store, StatePath: filepath.Join(stateDir, \"server.json\"), QueueWorkers: workers})\nif err != nil { log.Fatal(err) }\n}\n")
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return fmt.Errorf("format generated server: %w\n%s", err, source.String())
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(output, formatted, 0o600); err != nil {
		return fmt.Errorf("write generated server: %w", err)
	}
	return nil
}
