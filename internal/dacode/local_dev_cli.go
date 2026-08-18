package dacode

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago/daconfig"
)

type localDevRuntime struct {
	server     *localDevServer
	controller restartController
}

// newLocalDevRuntime compiles already-validated CLI configuration without I/O.
func newLocalDevRuntime(options cliOptions, serverConfig daconfig.ServerConfig) *localDevRuntime {
	if options.localDevExecutable == "" {
		return nil
	}
	server := newLocalDevServer(options.localDevExecutable, options.localDevArguments, localDevServerConfig{
		Endpoint: options.localDevEndpoint, HealthPath: options.localDevHealthPath,
		Directory: serverConfig.WorkingDirectory, Environment: serverConfig.Environment(),
		InheritEnvironment: options.localDevInheritEnvironment,
	})
	return &localDevRuntime{server: server, controller: newLocalDevRestartController(server)}
}

func (runtime *localDevRuntime) Start(ctx context.Context) error {
	if runtime == nil || runtime.server == nil {
		return errors.New("local development server runtime is unavailable")
	}
	return runtime.server.Start(ctx)
}

func (runtime *localDevRuntime) Close(ctx context.Context) error {
	if runtime == nil || runtime.server == nil {
		return nil
	}
	return runtime.server.Close(ctx)
}

func (runtime *localDevRuntime) RestartController() restartController {
	if runtime == nil {
		return nil
	}
	return runtime.controller
}

func validateLocalDevCLIOptions(options cliOptions, explicit map[string]bool) error {
	related := len(options.localDevArguments) > 0 || len(options.localDevInheritEnvironment) > 0 ||
		explicit["local-dev-endpoint"] || explicit["local-dev-health-path"]
	if options.localDevExecutable == "" {
		if related {
			return errors.New("--local-dev-arg, --local-dev-endpoint, --local-dev-health-path, and --local-dev-inherit-env require --local-dev-server")
		}
		return nil
	}
	if !filepath.IsAbs(options.localDevExecutable) || strings.IndexByte(options.localDevExecutable, 0) >= 0 {
		return errors.New("--local-dev-server must be an absolute executable path")
	}
	if len(options.localDevArguments) > maxLocalDevArguments {
		return fmt.Errorf("--local-dev-arg may be repeated at most %d times", maxLocalDevArguments)
	}
	for _, argument := range options.localDevArguments {
		if strings.IndexByte(argument, 0) >= 0 || len(argument) > maxLocalDevArgumentBytes {
			return fmt.Errorf("--local-dev-arg values must not contain NUL and must be at most %d bytes", maxLocalDevArgumentBytes)
		}
	}
	if len(options.localDevInheritEnvironment) > maxLocalDevEnvironment {
		return fmt.Errorf("--local-dev-inherit-env may be repeated at most %d times", maxLocalDevEnvironment)
	}
	for _, key := range options.localDevInheritEnvironment {
		if err := validateLocalDevEnvironmentKeyForCLI(key); err != nil {
			return fmt.Errorf("--local-dev-inherit-env %q: %w", key, err)
		}
	}
	if _, err := validateLocalDevEndpoint(options.localDevEndpoint, options.localDevHealthPath); err != nil {
		return fmt.Errorf("local development server: %w", err)
	}
	return nil
}

func validateLocalDevEnvironmentKeyForCLI(key string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	validateLocalDevEnvironmentKey(key)
	return nil
}
