package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type generated struct {
	Generated    bool            `json:"generated"`
	SourceSHA256 string          `json:"source_sha256"`
	Contract     json.RawMessage `json:"contract"`
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
