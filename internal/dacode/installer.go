package dacode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/semistrict/dago/dainstall"
)

func dacodeInstallCatalog() []dainstall.Spec {
	entries := []struct{ name, description string }{
		{"agentcore", "Included Go package: dabackend/agentcore"},
		{"contexthub", "Included Go package: dabackend/contexthub"},
		{"daytona", "Included Go package: dabackend/daytona"},
		{"docker", "Included Go package: dabackend/docker"},
		{"langsmith", "Included Go package: dabackend/langsmith"},
		{"media", "Included Go package: davideo"},
		{"modal", "Included Go package: dabackend/modal"},
		{"nemotron", "Included Go package: daproviders/nemotron"},
		{"nvidia", "Included profiles: daproviders/profile"},
		{"ollama", "Included Go package: daproviders/ollama"},
		{"openai", "Included Go package: daproviders/openai"},
		{"openrouter", "Included Go package: daproviders/openrouter"},
		{"quickjs", "Included API: dago.WithInterpreter"},
		{"runloop", "Included Go package: dabackend/runloop"},
		{"vercel", "Included Go package: dabackend/vercel"},
	}
	result := make([]dainstall.Spec, len(entries))
	for index, entry := range entries {
		result[index] = dainstall.Spec{Name: entry.name, Kind: dainstall.Extra, Description: entry.description, BuiltIn: true}
	}
	return result
}

func runInstallCommand(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	installer := dainstall.New(dainstall.OSExecutor(), dacodeInstallCatalog(), dainstall.Options{})
	return executeInstallCommand(ctx, arguments, stdin, stdout, stderr, installer)
}

type installCommandOptions struct {
	kind            dainstall.Kind
	name            string
	yes, json, help bool
}

func parseInstallArguments(arguments []string) (installCommandOptions, error) {
	options := installCommandOptions{kind: dainstall.Extra}
	for _, argument := range arguments {
		switch argument {
		case "--package":
			options.kind = dainstall.Package
		case "--yes", "-y", "--force":
			options.yes = true
		case "--json":
			options.json = true
		case "--help", "-h":
			options.help = true
		default:
			if strings.HasPrefix(argument, "-") {
				return installCommandOptions{}, fmt.Errorf("unknown install option %q", argument)
			}
			if options.name != "" {
				return installCommandOptions{}, errors.New("install accepts exactly one dependency name")
			}
			options.name = argument
		}
	}
	return options, nil
}

func executeInstallCommand(ctx context.Context, arguments []string, stdin io.Reader, stdout, _ io.Writer, installer *dainstall.Installer) error {
	options, err := parseInstallArguments(arguments)
	if err != nil {
		return &commandExitError{code: 2, err: err}
	}
	if options.help || options.name == "" {
		printInstallUsage(stdout, installer)
		if options.help {
			return nil
		}
		return &commandExitError{code: 2, err: errors.New("install requires one allowlisted dependency name")}
	}
	entry, found := installEntry(installer.Available(options.kind), strings.ToLower(options.name))
	if !found {
		return &commandExitError{code: 2, err: fmt.Errorf("%w: %s %q", dainstall.ErrUnknownDependency, options.kind, safeCLIValue(options.name))}
	}
	authorization := dainstall.AuthorizationDenied
	if !entry.BuiltIn {
		if options.yes {
			authorization = dainstall.AuthorizationGranted
		} else {
			fmt.Fprintf(stdout, "Installing %s %q runs the allowlisted package installer and third-party build code. Continue? [y/N] ", options.kind, entry.Name)
			approved, readErr := readInstallConfirmation(stdin)
			if readErr != nil {
				return &commandExitError{code: 2, err: readErr}
			}
			if !approved {
				return &commandExitError{code: 1, err: errors.New("install canceled")}
			}
			authorization = dainstall.AuthorizationGranted
		}
	}
	result, err := installer.Install(ctx, options.kind, entry.Name, authorization)
	if err != nil {
		return err
	}
	if options.json {
		return json.NewEncoder(stdout).Encode(map[string]any{"schema_version": 1, "command": "install", "data": result})
	}
	if result.Status == dainstall.AlreadyAvailable {
		_, err = fmt.Fprintf(stdout, "%s %q is already included in this dago release; import and configure it in your application.\n", result.Kind, result.Name)
	} else {
		_, err = fmt.Fprintf(stdout, "Installed %s %q. Restart dacode before using it.\n", result.Kind, result.Name)
	}
	return err
}

func installEntry(entries []dainstall.Entry, name string) (dainstall.Entry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return dainstall.Entry{}, false
}

func readInstallConfirmation(input io.Reader) (bool, error) {
	reader := bufio.NewReaderSize(io.LimitReader(input, 17), 17)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, errors.New("read install confirmation")
	}
	if len(value) > 16 {
		return false, errors.New("install confirmation is too long")
	}
	answer := strings.ToLower(strings.TrimSpace(value))
	return answer == "y" || answer == "yes", nil
}

func safeCLIValue(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character >= 0x20 && character <= 0x7e {
			builder.WriteRune(character)
		} else {
			builder.WriteString(strconv.QuoteRuneToASCII(character))
		}
		if builder.Len() > 128 {
			builder.WriteString("...")
			break
		}
	}
	value = builder.String()
	if len(value) > 128 {
		return value[:128] + "..."
	}
	return value
}

func printInstallUsage(output io.Writer, installer *dainstall.Installer) {
	fmt.Fprintln(output, "Usage: dacode install NAME [--package] [--force|--yes] [--json]")
	fmt.Fprintln(output, "Check only integrations explicitly allowlisted by this build.")
	fmt.Fprintln(output, "Included Go integration extras:")
	for _, entry := range installer.Available(dainstall.Extra) {
		fmt.Fprintf(output, "  %-12s %s\n", entry.Name, entry.Description)
	}
	if len(installer.Available(dainstall.Package)) == 0 {
		fmt.Fprintln(output, "Allowlisted external packages: none in this build.")
	}
}
