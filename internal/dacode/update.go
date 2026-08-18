package dacode

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/daupdate"
)

type updateMode string

const (
	updateCheck  updateMode = "check"
	updateDryRun updateMode = "dry-run"
	updateApply  updateMode = "apply"
)

type updateCommandOptions struct {
	channel, artifact       string
	manifestBase, publicKey string
	current, target         string
	mode                    updateMode
	json, help              bool
}

type updateService interface {
	Check(context.Context, string) (daupdate.Result, error)
	DryRun(context.Context, string) (daupdate.Result, error)
	Apply(context.Context, string, string, daupdate.Authorization) (daupdate.Result, error)
}

type updateServiceFactory func(updateCommandOptions) (updateService, error)

func runUpdateCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	return executeUpdateCommand(ctx, arguments, stdout, stderr, productionUpdateService)
}

func parseUpdateArguments(arguments []string) (updateCommandOptions, error) {
	options := updateCommandOptions{mode: updateCheck, current: dago.Version()}
	positionals := []string{}
	selectedMode := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		value := func() (string, error) {
			if index+1 >= len(arguments) {
				return "", fmt.Errorf("update option %q requires a value", argument)
			}
			index++
			return arguments[index], nil
		}
		switch argument {
		case "--manifest-base":
			parsed, err := value()
			if err != nil {
				return updateCommandOptions{}, err
			}
			options.manifestBase = parsed
		case "--public-key":
			parsed, err := value()
			if err != nil {
				return updateCommandOptions{}, err
			}
			options.publicKey = parsed
		case "--current":
			parsed, err := value()
			if err != nil {
				return updateCommandOptions{}, err
			}
			options.current = parsed
		case "--target":
			parsed, err := value()
			if err != nil {
				return updateCommandOptions{}, err
			}
			options.target = parsed
		case "--check", "--dry-run", "--apply":
			if selectedMode {
				return updateCommandOptions{}, errors.New("update accepts exactly one of --check, --dry-run, or --apply")
			}
			selectedMode = true
			options.mode = updateMode(strings.TrimPrefix(argument, "--"))
		case "--json":
			options.json = true
		case "--help", "-h":
			options.help = true
		default:
			if strings.HasPrefix(argument, "-") {
				return updateCommandOptions{}, errors.New("unknown update option")
			}
			positionals = append(positionals, argument)
		}
	}
	if options.help {
		return options, nil
	}
	if len(positionals) != 2 {
		return updateCommandOptions{}, errors.New("update requires CHANNEL and ARTIFACT positional arguments")
	}
	options.channel, options.artifact = positionals[0], positionals[1]
	if strings.TrimSpace(options.manifestBase) == "" || strings.TrimSpace(options.publicKey) == "" {
		return updateCommandOptions{}, errors.New("update requires --manifest-base and --public-key")
	}
	if options.target != "" && options.mode != updateApply {
		return updateCommandOptions{}, errors.New("--target requires --apply")
	}
	return options, nil
}

func executeUpdateCommand(ctx context.Context, arguments []string, stdout, _ io.Writer, factory updateServiceFactory) error {
	options, err := parseUpdateArguments(arguments)
	if err != nil {
		return &commandExitError{code: 2, err: err}
	}
	if options.help {
		printUpdateUsage(stdout)
		return nil
	}
	service, err := factory(options)
	if err != nil {
		return err
	}
	var result daupdate.Result
	switch options.mode {
	case updateCheck:
		result, err = service.Check(ctx, options.current)
	case updateDryRun:
		result, err = service.DryRun(ctx, options.current)
	case updateApply:
		if options.target == "" {
			options.target, err = os.Executable()
			if err == nil {
				options.target, err = filepath.Abs(options.target)
			}
			if err != nil {
				return daupdate.ErrApplyFailed
			}
		}
		if runtime.GOOS == "windows" && sameExecutablePath(options.target) {
			return fmt.Errorf("%w: Windows requires --target to name an executable not running this process", daupdate.ErrApplyFailed)
		}
		result, err = service.Apply(ctx, options.current, options.target, daupdate.AuthorizationGranted)
	}
	if err != nil {
		return err
	}
	if options.json {
		return json.NewEncoder(stdout).Encode(map[string]any{"schema_version": 1, "command": "update", "mode": options.mode, "data": result})
	}
	return printUpdateResult(stdout, options.mode, result)
}

func productionUpdateService(options updateCommandOptions) (updateService, error) {
	key, err := readUpdatePublicKey(options.publicKey)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	var source *daupdate.HTTPSource
	func() {
		defer func() {
			if recover() != nil {
				source = nil
			}
		}()
		source = daupdate.NewHTTPSource(client, options.manifestBase)
	}()
	if source == nil {
		return nil, errors.New("invalid update manifest base URL")
	}
	var service updateService
	func() {
		defer func() {
			if recover() != nil {
				service = nil
			}
		}()
		service = daupdate.New(options.channel, options.artifact, key, source, daupdate.Options{})
	}()
	if service == nil {
		return nil, errors.New("invalid update channel or artifact")
	}
	return service, nil
}

func readUpdatePublicKey(path string) (ed25519.PublicKey, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, errors.New("update public key path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 256 || !trustedUpdateKey(info) {
		return nil, errors.New("read update public key")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("read update public key")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > 256 || !trustedUpdateKey(opened) {
		return nil, errors.New("read update public key")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 257))
	if err != nil || len(raw) > 256 {
		return nil, errors.New("read update public key")
	}
	encoded := strings.TrimSpace(string(raw))
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		decoded, err = base64.StdEncoding.Strict().DecodeString(encoded)
	}
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("update public key must be one Ed25519 key encoded as hex or base64")
	}
	return ed25519.PublicKey(decoded), nil
}

func sameExecutablePath(target string) bool {
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return false
	}
	target, err = filepath.Abs(target)
	return err == nil && strings.EqualFold(filepath.Clean(executable), filepath.Clean(target))
}

func printUpdateResult(output io.Writer, mode updateMode, result daupdate.Result) error {
	switch mode {
	case updateCheck:
		_, err := fmt.Fprintf(output, "%s: current %s, channel %s.\n", strings.ReplaceAll(string(result.Status), "_", " "), result.CurrentVersion, result.LatestVersion)
		return err
	case updateDryRun:
		if result.Status != daupdate.UpdateAvailable {
			_, err := fmt.Fprintf(output, "%s: current %s, channel %s.\n", strings.ReplaceAll(string(result.Status), "_", " "), result.CurrentVersion, result.LatestVersion)
			return err
		}
		_, err := fmt.Fprintf(output, "Verified %s %s for channel %s without changing the executable.\n", result.Artifact, result.LatestVersion, result.Channel)
		return err
	case updateApply:
		if !result.Applied {
			_, err := fmt.Fprintf(output, "%s: current %s, channel %s.\n", strings.ReplaceAll(string(result.Status), "_", " "), result.CurrentVersion, result.LatestVersion)
			return err
		}
		_, err := fmt.Fprintf(output, "Updated %s to %s. Restart the executable to use this release.\n", result.Artifact, result.LatestVersion)
		return err
	default:
		return errors.New("invalid update mode")
	}
}

func printUpdateUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: dacode update CHANNEL ARTIFACT --manifest-base URL --public-key PATH [MODE] [OPTIONS]")
	fmt.Fprintln(output, "Modes: --check (default), --dry-run, --apply")
	fmt.Fprintln(output, "A signed Ed25519 manifest and matching SHA-256 artifact are mandatory.")
	fmt.Fprintln(output, "--dry-run downloads and verifies without writing; --apply atomically replaces --target or this executable.")
	fmt.Fprintln(output, "Development builds need an explicit semver --current value.")
}
