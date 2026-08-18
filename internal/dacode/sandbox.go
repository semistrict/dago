package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dasandbox"
)

const (
	configuredSandboxSentinel = "__configured_default__"
	maxSandboxSetupFileBytes  = 1 << 20
)

func normalizeOptionalSandboxFlag(arguments []string) []string {
	result := append([]string(nil), arguments...)
	for index, argument := range result {
		if argument == "--sandbox" && (index+1 == len(result) || strings.HasPrefix(result[index+1], "-")) {
			result[index] = "--sandbox=" + configuredSandboxSentinel
		}
	}
	return result
}

func openSandboxSession(ctx context.Context, registry *dasandbox.Registry, workingDir string, options cliOptions) (*dasandbox.Session, error) {
	if options.sandbox == "" {
		return nil, nil
	}
	if registry == nil {
		return nil, errors.New("remote sandbox registry is unavailable")
	}
	provider := options.sandbox
	if provider == configuredSandboxSentinel {
		provider = strings.TrimSpace(options.sandboxDefault)
		if provider == "" {
			provider = registry.Default()
		}
		if provider == "" {
			return nil, errors.New("--sandbox requires a provider or configured sandboxes.default")
		}
	}
	var setup []byte
	if options.sandboxSetup != "" {
		root, err := os.OpenRoot(workingDir)
		if err != nil {
			return nil, fmt.Errorf("open sandbox setup root: %w", err)
		}
		defer root.Close()
		setupPath := options.sandboxSetup
		if filepath.IsAbs(setupPath) {
			relative, relErr := filepath.Rel(workingDir, setupPath)
			if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return nil, errors.New("sandbox setup file must be within the workspace")
			}
			setupPath = relative
		}
		text, err := readRootFile(root, setupPath, maxSandboxSetupFileBytes)
		if err != nil {
			return nil, fmt.Errorf("read sandbox setup file: %w", err)
		}
		setup = []byte(text)
	}
	return registry.Open(ctx, provider, dasandbox.OpenRequest{
		SandboxID: options.sandboxID, Snapshot: options.sandboxSnapshot, SetupScript: setup,
	})
}

type sandboxSessionCloser struct{ session *dasandbox.Session }

func (closer sandboxSessionCloser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return closer.session.Close(ctx)
}

var _ io.Closer = sandboxSessionCloser{}

func sandboxBackend(session *dasandbox.Session) dabackend.Backend {
	if session == nil {
		return nil
	}
	return session
}
