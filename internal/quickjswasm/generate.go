//go:build ignore

// Command generate downloads the pinned quickjs-rs execution guest and builds
// dago's source-controlled fork of its OXC transform guest. The fork adds the
// workflow-module grammar while retaining the upstream transform behavior.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/semistrict/dago/internal/wafl"
)

const (
	wheelURL = "https://files.pythonhosted.org/packages/d0/1d/e4406d13ce9b9443dbfa59e2a2d5b3e11278ebe322b54de38ae18faf5436/quickjs_rs-0.2.5-py3-none-any.whl"
	wheelSHA = "e82240af1f1dd1b2e12bcf169a22a8e0e451e356f0688f2fc3bba886d9b2bb20"
)

func main() {
	client := &http.Client{Timeout: 2 * time.Minute}
	response, err := client.Get(wheelURL) //nolint:gosec // URL and digest are pinned.
	if err != nil {
		fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fatal(fmt.Errorf("download wheel: %s", response.Status))
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != wheelSHA {
		fatal(fmt.Errorf("wheel sha256 = %s, want %s", got, wheelSHA))
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		fatal(err)
	}
	extracted := map[string][]byte{}
	for _, artifact := range []string{"_guest.wasm"} {
		name := "quickjs_rs/" + artifact
		var source *zip.File
		for _, file := range reader.File {
			if file.Name == name {
				source = file
				break
			}
		}
		if source == nil {
			fatal(fmt.Errorf("wheel is missing %s", name))
		}
		input, err := source.Open()
		if err != nil {
			fatal(err)
		}
		contents, err := io.ReadAll(input)
		closeErr := input.Close()
		if err != nil {
			fatal(err)
		}
		if closeErr != nil {
			fatal(closeErr)
		}
		if err := os.WriteFile(filepath.Clean(artifact), contents, 0o644); err != nil {
			fatal(err)
		}
		extracted[artifact] = contents
	}
	transform, err := buildTransform()
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile("_transform.wasm", transform, 0o644); err != nil {
		fatal(err)
	}
	tracked, err := wafl.Transform(extracted["_guest.wasm"], wafl.Config{PageSize: 4096})
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile("_guest_tracked.wasm", tracked, 0o644); err != nil {
		fatal(err)
	}
}

func buildTransform() ([]byte, error) {
	target, err := os.MkdirTemp("", "dago-quickjs-transform-")
	if err != nil {
		return nil, fmt.Errorf("create transform build directory: %w", err)
	}
	defer os.RemoveAll(target)
	manifest, err := filepath.Abs(filepath.Join("transform", "Cargo.toml"))
	if err != nil {
		return nil, fmt.Errorf("resolve transform manifest: %w", err)
	}
	command := exec.Command(
		"cargo", "build", "--locked", "--release", "--target", "wasm32-unknown-unknown",
		"--manifest-path", manifest,
	)
	command.Dir = filepath.Dir(manifest)
	command.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("build workflow source transformer: %w", err)
	}
	artifact := filepath.Join(target, "wasm32-unknown-unknown", "release", "dago_quickjs_wasm_transform.wasm")
	contents, err := os.ReadFile(artifact)
	if err != nil {
		return nil, fmt.Errorf("read workflow source transformer: %w", err)
	}
	return contents, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
