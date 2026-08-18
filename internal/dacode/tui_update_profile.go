package dacode

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/semistrict/dago"
)

// tuiUpdateProfile is the explicit launch-time trust boundary for interactive
// update checks and activation. It never infers release origins or keys.
type tuiUpdateProfile struct {
	service         updateService
	current         string
	target          string
	operatingSystem string
}

func (profile *tuiUpdateProfile) platform() string {
	if profile != nil && profile.operatingSystem != "" {
		return profile.operatingSystem
	}
	return runtime.GOOS
}

func validateTUIUpdateOptions(options cliOptions) error {
	values := []string{options.updateChannel, options.updateArtifact, options.updateManifestBase, options.updatePublicKey}
	configured := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			configured++
		}
	}
	if configured != 0 && configured != len(values) {
		return errors.New("--update-channel, --update-artifact, --update-manifest-base, and --update-public-key must be provided together")
	}
	if strings.TrimSpace(options.updateTarget) != "" && configured == 0 {
		return errors.New("--update-target requires a complete update profile")
	}
	if options.updateTarget != "" && (!filepath.IsAbs(options.updateTarget) || strings.ContainsRune(options.updateTarget, 0)) {
		return errors.New("--update-target must be an absolute path")
	}
	return nil
}

func configuredTUIUpdateProfile(options cliOptions) (*tuiUpdateProfile, error) {
	if strings.TrimSpace(options.updateChannel) == "" {
		return nil, nil
	}
	service, err := productionUpdateService(updateCommandOptions{
		channel: options.updateChannel, artifact: options.updateArtifact,
		manifestBase: options.updateManifestBase, publicKey: options.updatePublicKey,
		current: dago.Version(), target: options.updateTarget,
	})
	if err != nil {
		return nil, err
	}
	return &tuiUpdateProfile{service: service, current: dago.Version(), target: options.updateTarget, operatingSystem: runtime.GOOS}, nil
}
