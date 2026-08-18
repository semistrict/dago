package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dadoctor"
)

type doctorCommandOptions struct {
	json     bool
	ascii    bool
	config   string
	stateDir string
	workDir  string
}

func runDoctorCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	options, help, err := parseDoctorArguments(arguments)
	if err != nil {
		return &commandExitError{code: 2, err: err}
	}
	if help {
		printDoctorUsage(stdout)
		return nil
	}
	configPath, err := doctorConfigPath(options.config)
	if err != nil {
		return err
	}
	dataDirectory := options.stateDir
	if dataDirectory == "" {
		configDirectory, configErr := os.UserConfigDir()
		if configErr == nil {
			dataDirectory = filepath.Join(configDirectory, "dacode")
		}
	}
	doctorOptions := dadoctor.Options{
		WorkingDirectory: options.workDir,
		ConfigPath:       configPath,
		DataDirectory:    dataDirectory,
		RuntimeVersions:  []dadoctor.Version{{Name: "dago (SDK)", Value: dago.Version()}},
		Commit:           dadoctor.BuildCommit(),
	}
	if dataDirectory != "" {
		doctorOptions.CredentialFiles = []string{filepath.Join(dataDirectory, oauthStoreFilename)}
	}
	doctor := dadoctor.New("dacode", buildVersion(), dadoctor.OSSystem(), doctorOptions)
	report, err := doctor.Collect(ctx)
	if err != nil {
		return err
	}
	if options.json {
		err = dadoctor.WriteJSON(stdout, report)
	} else {
		err = dadoctor.WriteText(stdout, report, options.ascii)
	}
	if err != nil {
		return err
	}
	if !report.Healthy {
		return &silentCommandExitError{commandExitError{code: 1, err: errors.New("doctor found unhealthy diagnostics")}}
	}
	return nil
}

func doctorConfigPath(explicit string) (string, error) {
	path := explicit
	if path == "" {
		for _, name := range []string{"DACODE_CONFIG", "DEEPAGENTS_CODE_DACODE_CONFIG", "DEEPAGENTS_CLI_DACODE_CONFIG"} {
			if value, ok := os.LookupEnv(name); ok {
				path = value
			}
		}
	}
	if len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", errors.New("config path is invalid")
	}
	if path == "" {
		directory, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve config directory: %w", err)
		}
		return filepath.Join(directory, "dacode", "config.json"), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return absolute, nil
}

func parseDoctorArguments(arguments []string) (doctorCommandOptions, bool, error) {
	options := doctorCommandOptions{workDir: "."}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--json":
			options.json = true
		case argument == "--ascii":
			options.ascii = true
		case argument == "--help" || argument == "-h":
			return options, true, nil
		case argument == "--config" || argument == "--state-dir" || argument == "--cwd":
			index++
			if index >= len(arguments) || strings.TrimSpace(arguments[index]) == "" {
				return doctorCommandOptions{}, false, fmt.Errorf("%s requires one non-empty path", argument)
			}
			switch argument {
			case "--config":
				options.config = arguments[index]
			case "--state-dir":
				options.stateDir = arguments[index]
			case "--cwd":
				options.workDir = arguments[index]
			}
		case strings.HasPrefix(argument, "--config="):
			options.config = strings.TrimPrefix(argument, "--config=")
		case strings.HasPrefix(argument, "--state-dir="):
			options.stateDir = strings.TrimPrefix(argument, "--state-dir=")
		case strings.HasPrefix(argument, "--cwd="):
			options.workDir = strings.TrimPrefix(argument, "--cwd=")
		default:
			return doctorCommandOptions{}, false, fmt.Errorf("unknown doctor option %q", argument)
		}
	}
	if options.config == "" && containsDoctorEmptyPath(arguments, "--config=") {
		return doctorCommandOptions{}, false, errors.New("--config requires one non-empty path")
	}
	if options.stateDir == "" && containsDoctorEmptyPath(arguments, "--state-dir=") {
		return doctorCommandOptions{}, false, errors.New("--state-dir requires one non-empty path")
	}
	if options.workDir == "" {
		return doctorCommandOptions{}, false, errors.New("--cwd requires one non-empty path")
	}
	return options, false, nil
}

func containsDoctorEmptyPath(arguments []string, prefix string) bool {
	for _, argument := range arguments {
		if argument == prefix {
			return true
		}
	}
	return false
}

func printDoctorUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: dacode doctor [--json] [--ascii] [--config PATH] [--state-dir PATH] [--cwd PATH]")
	fmt.Fprintln(output, "Print bounded, offline install and configuration diagnostics safe for support reports.")
}
