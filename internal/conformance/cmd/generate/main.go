package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type generated struct {
	Generated    bool            `json:"generated"`
	SourceSHA256 string          `json:"source_sha256"`
	Contract     json.RawMessage `json:"contract"`
}

type contractSource struct {
	SchemaVersion int               `json:"schema_version"`
	Provenance    []provenanceEntry `json:"provenance"`
	Message       json.RawMessage   `json:"message"`
	Tool          json.RawMessage   `json:"tool"`
	ModelResponse json.RawMessage   `json:"model_response"`
	StateUpdate   json.RawMessage   `json:"state_update"`
	Checkpoint    json.RawMessage   `json:"checkpoint"`
	StreamEvent   json.RawMessage   `json:"stream_event"`
}

type provenanceEntry struct {
	Source        string   `json:"source"`
	Path          string   `json:"path"`
	TestSelectors []string `json:"test_selectors"`
}

type upstreamManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Sources       []upstreamSource `json:"sources"`
}

type upstreamSource struct {
	Name        string   `json:"name"`
	Repository  string   `json:"repository"`
	Revision    string   `json:"revision"`
	Version     string   `json:"version,omitempty"`
	License     string   `json:"license"`
	CheckoutEnv string   `json:"checkout_env,omitempty"`
	Surfaces    []string `json:"surfaces"`
}

func main() {
	check := flag.Bool("check", false, "fail if generated output is stale")
	flag.Parse()
	root, err := findRoot()
	must(err)
	sourcePath := filepath.Join(root, "conformance", "contracts.source.json")
	outputPath := filepath.Join(root, "internal", "conformance", "testdata", "contracts.v1.json")
	source, err := os.ReadFile(sourcePath)
	must(err)
	var contractDocument contractSource
	must(decodeStrict(source, &contractDocument))
	must(validateContractSource(contractDocument))
	manifestData, err := os.ReadFile(filepath.Join(root, "docs", "upstream-manifest.json"))
	must(err)
	var manifest upstreamManifest
	must(decodeStrict(manifestData, &manifest))
	must(validateProvenance(contractDocument.Provenance, manifest, os.Getenv, runGit))
	var normalized any
	must(json.Unmarshal(source, &normalized))
	contract, err := json.Marshal(normalized)
	must(err)
	digest := sha256.Sum256(source)
	payload, err := json.MarshalIndent(generated{Generated: true, SourceSHA256: hex.EncodeToString(digest[:]), Contract: contract}, "", "  ")
	must(err)
	payload = append(payload, '\n')
	if *check {
		current, err := os.ReadFile(outputPath)
		must(err)
		if !bytes.Equal(current, payload) {
			fmt.Fprintln(os.Stderr, "generated conformance fixtures are stale; run make generate")
			os.Exit(1)
		}
		return
	}
	must(os.MkdirAll(filepath.Dir(outputPath), 0o755))
	must(os.WriteFile(outputPath, payload, 0o644))
}

func validateContractSource(source contractSource) error {
	if source.SchemaVersion != 1 {
		return fmt.Errorf("unsupported conformance source schema version %d", source.SchemaVersion)
	}
	required := map[string]json.RawMessage{
		"message": source.Message, "tool": source.Tool, "model_response": source.ModelResponse,
		"state_update": source.StateUpdate, "checkpoint": source.Checkpoint,
		"stream_event": source.StreamEvent,
	}
	for name, value := range required {
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("conformance contract %q is required", name)
		}
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

type environmentLookup func(string) string
type gitCommand func(...string) ([]byte, error)

func validateProvenance(provenance []provenanceEntry, manifest upstreamManifest, getenv environmentLookup, git gitCommand) error {
	if len(provenance) == 0 {
		return fmt.Errorf("conformance provenance is required")
	}
	sources := make(map[string]upstreamSource, len(manifest.Sources))
	for _, source := range manifest.Sources {
		sources[source.Name] = source
	}
	for index, entry := range provenance {
		source, ok := sources[entry.Source]
		if !ok {
			return fmt.Errorf("conformance provenance entry %d source %q is not in the upstream manifest", index, entry.Source)
		}
		if err := validateProvenancePath(entry.Path); err != nil {
			return fmt.Errorf("conformance provenance source %q: %w", entry.Source, err)
		}
		if len(entry.TestSelectors) == 0 {
			return fmt.Errorf("conformance provenance source %q path %q has no test selectors", entry.Source, entry.Path)
		}
		seenSelectors := make(map[string]struct{}, len(entry.TestSelectors))
		for _, selector := range entry.TestSelectors {
			if err := validateTestSelector(selector); err != nil {
				return fmt.Errorf("conformance provenance source %q path %q: %w", entry.Source, entry.Path, err)
			}
			if _, duplicate := seenSelectors[selector]; duplicate {
				return fmt.Errorf("conformance provenance source %q path %q repeats test selector %q", entry.Source, entry.Path, selector)
			}
			seenSelectors[selector] = struct{}{}
		}
		if source.Revision == "" {
			return fmt.Errorf("upstream manifest source %q has no revision", entry.Source)
		}
		if source.CheckoutEnv == "" {
			continue
		}
		checkout := getenv(source.CheckoutEnv)
		if checkout == "" {
			continue
		}
		head, err := git("-C", checkout, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("inspect %s checkout for %q: %w", source.CheckoutEnv, entry.Source, err)
		}
		if strings.TrimSpace(string(head)) != source.Revision {
			return fmt.Errorf("%s checkout for %q is at %s, want %s", source.CheckoutEnv, entry.Source, strings.TrimSpace(string(head)), source.Revision)
		}
		contents, err := git("-C", checkout, "show", source.Revision+":"+entry.Path)
		if err != nil {
			return fmt.Errorf("provenance path %q does not exist at revision %s in %s: %w", entry.Path, source.Revision, source.CheckoutEnv, err)
		}
		for _, selector := range entry.TestSelectors {
			if !pythonTestSelectorExists(contents, selector) {
				return fmt.Errorf("test selector %q does not exist in provenance path %q at revision %s", selector, entry.Path, source.Revision)
			}
		}
	}
	return nil
}

func validateTestSelector(selector string) error {
	parts := strings.Split(selector, "::")
	if len(parts) < 1 || len(parts) > 2 {
		return fmt.Errorf("test selector %q must name a test function or TestClass::test_method", selector)
	}
	for _, part := range parts {
		if !isPythonIdentifier(part) {
			return fmt.Errorf("test selector %q contains an invalid Python identifier", selector)
		}
	}
	if len(parts) == 2 && !strings.HasPrefix(parts[0], "Test") {
		return fmt.Errorf("test selector %q class must start with Test", selector)
	}
	if !strings.HasPrefix(parts[len(parts)-1], "test_") {
		return fmt.Errorf("test selector %q test name must start with test_", selector)
	}
	return nil
}

func isPythonIdentifier(value string) bool {
	if value == "" || !isPythonIdentifierStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !isPythonIdentifierStart(character) && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func isPythonIdentifierStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func pythonTestSelectorExists(contents []byte, selector string) bool {
	parts := strings.Split(selector, "::")
	if len(parts) == 1 {
		pattern := `(?m)^(?:async[ \t]+)?def[ \t]+` + regexp.QuoteMeta(parts[0]) + `[ \t]*\(`
		return regexp.MustCompile(pattern).Find(contents) != nil
	}
	classPattern := regexp.MustCompile(`(?m)^class[ \t]+` + regexp.QuoteMeta(parts[0]) + `(?:[ \t]*\([^\n]*\))?[ \t]*:`)
	location := classPattern.FindIndex(contents)
	if location == nil {
		return false
	}
	lines := strings.Split(string(contents[location[1]:]), "\n")
	directIndent := -1
	classEnd := len(lines)
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := pythonIndentWidth(line)
		if indent == 0 {
			classEnd = index
			break
		}
		if directIndent < 0 || indent < directIndent {
			directIndent = indent
		}
	}
	if directIndent < 0 {
		return false
	}
	methodPattern := regexp.MustCompile(`^(?:async[ \t]+)?def[ \t]+` + regexp.QuoteMeta(parts[1]) + `[ \t]*\(`)
	for _, line := range lines[:classEnd] {
		if pythonIndentWidth(line) != directIndent {
			continue
		}
		if methodPattern.MatchString(strings.TrimLeft(line, " \t")) {
			return true
		}
	}
	return false
}

func pythonIndentWidth(line string) int {
	for index, character := range []byte(line) {
		if character != ' ' && character != '\t' {
			return index
		}
	}
	return len(line)
}

func validateProvenancePath(path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	if filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q must be a clean repository-relative path", path)
	}
	return nil
}

func runGit(arguments ...string) ([]byte, error) {
	return exec.Command("git", arguments...).CombinedOutput()
}

func findRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("repository root not found")
		}
		directory = parent
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
