//go:build ignore

// Command generate downloads the pinned quickjs-rs wheel and extracts the
// two generated WASM guests. The wheel is the canonical artifact consumed by
// Deep Agents, so this intentionally does not rebuild it with a local Rust
// toolchain.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
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
	for _, artifact := range []string{"_guest.wasm", "_transform.wasm"} {
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
	tracked, err := wafl.Transform(extracted["_guest.wasm"], wafl.Config{PageSize: 4096})
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile("_guest_tracked.wasm", tracked, 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
