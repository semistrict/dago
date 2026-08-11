package claudetool

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"

	"mvdan.cc/sh/v3/syntax"

	"github.com/semistrict/dago/examples/shelley/claudetool/bashkit"
	"github.com/semistrict/dago/examples/shelley/llm/llmhttp"
)

// PermissionCallback applies Shelley's application policy to its yielding shell.
type PermissionCallback func(command string) error

// PreferredToolModels orders models used for the yielding shell's optional
// just-in-time package validation.
var PreferredToolModels = []string{"gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.4-mini", "predictable"}

const NoBashToolJITInstall = false

func isNoTrailerSet() bool {
	out, err := exec.Command("git", "config", "--get", "shelley.no-trailer").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

type commandInstaller struct {
	LLMProvider LLMServiceProvider
}

var (
	autoInstallMu           sync.Mutex
	doNotAttemptToolInstall = make(map[string]bool)
)

func (installer commandInstaller) checkAndInstallMissingTools(ctx context.Context, command string) error {
	commands, err := bashkit.ExtractCommands(command)
	if err != nil {
		return err
	}
	autoInstallMu.Lock()
	defer autoInstallMu.Unlock()
	for _, name := range commands {
		if doNotAttemptToolInstall[name] {
			continue
		}
		if shellHasCommand(ctx, name) {
			doNotAttemptToolInstall[name] = true
			continue
		}
		if err := installer.installTool(ctx, name); err != nil {
			slog.WarnContext(ctx, "failed to install tool", "tool", name, "error", err)
		}
		doNotAttemptToolInstall[name] = true
	}
	return nil
}

func shellHasCommand(ctx context.Context, name string) bool {
	quoted, err := syntax.Quote(name, syntax.LangBash)
	if err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, "bash", "--login", "-c", "command -v "+quoted)
	cmd.Stdin = nil
	return cmd.Run() == nil
}

func autodetectPackageManager() string {
	managers := []string{
		"apt", "apt-get", "brew", "port", "apk", "yum", "dnf", "pacman",
		"zypper", "xbps-install", "emerge", "nix-env", "guix", "pkg", "slackpkg",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, name := range managers {
		if shellHasCommand(ctx, name) {
			return name
		}
	}
	return ""
}

func (installer commandInstaller) installTool(ctx context.Context, command string) error {
	slog.InfoContext(ctx, "attempting to install tool", "tool", command)
	manager := autodetectPackageManager()
	if manager == "" {
		return fmt.Errorf("no known package manager found in PATH")
	}
	chat, err := installer.selectBestChat()
	if err != nil {
		return fmt.Errorf("failed to get chat model for tool validation: %w", err)
	}
	response, err := chat.Invoke(llmhttp.WithPurpose(ctx, "tool_install"), damodel.Request{Messages: []dmessage.Message{
		dmessage.System("You are an expert in software developer tools."),
		dmessage.Human(toolInstallQuery(manager, command)),
	}})
	if err != nil {
		return fmt.Errorf("failed to validate tool with LLM: %w", err)
	}
	return installer.finishToolInstall(ctx, command, manager, response.Message.TextContent())
}

func (installer commandInstaller) selectBestChat() (damodel.Chat, error) {
	if installer.LLMProvider == nil {
		return nil, fmt.Errorf("no LLM provider available")
	}
	for _, modelID := range PreferredToolModels {
		if chat, err := installer.LLMProvider.GetChat(modelID); err == nil {
			return chat, nil
		}
	}
	available := installer.LLMProvider.GetAvailableModels()
	if len(available) == 0 {
		return nil, fmt.Errorf("no chat models available")
	}
	return installer.LLMProvider.GetChat(available[0])
}

func toolInstallQuery(packageManager, command string) string {
	return fmt.Sprintf(`Do you know this command/package/tool? Is it legitimate, clearly non-harmful, and commonly used? Can it be installed with package manager %s?

Command: %s

- YES: Respond ONLY with the package name used to install it
- NO or UNSURE: Respond ONLY with the word NO`, packageManager, command)
}

func (installer commandInstaller) finishToolInstall(ctx context.Context, command, manager, rawResponse string) error {
	response := strings.TrimSpace(rawResponse)
	if response == "NO" || response == "UNSURE" {
		return fmt.Errorf("tool %s not approved for installation", command)
	}
	if response == "" {
		return fmt.Errorf("no package name provided for tool %s", command)
	}
	if !validPackageName(response) {
		return fmt.Errorf("invalid package name returned for tool %s", command)
	}
	return installer.installPackage(ctx, command, response, manager)
}

func (installer commandInstaller) installPackage(ctx context.Context, command, packageName, manager string) error {
	updateCommand, installCommand, err := packageInstallCommands(manager, packageName)
	if err != nil {
		return err
	}
	if updateCommand.name != "" {
		if output, runErr := exec.CommandContext(ctx, updateCommand.name, updateCommand.args...).CombinedOutput(); runErr != nil {
			slog.WarnContext(ctx, "package cache update failed, proceeding with install anyway", "error", runErr, "output", string(output))
		}
	}
	output, err := exec.CommandContext(ctx, installCommand.name, installCommand.args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to install %s: %w\nOutput: %s", packageName, err, string(output))
	}
	slog.InfoContext(ctx, "tool installation successful", "tool", command, "package", packageName)
	return nil
}

type packageCommand struct {
	name string
	args []string
}

func packageInstallCommands(manager, packageName string) (packageCommand, packageCommand, error) {
	if !validPackageName(packageName) {
		return packageCommand{}, packageCommand{}, fmt.Errorf("invalid package name")
	}
	var updateCommand, installCommand packageCommand
	switch manager {
	case "apt", "apt-get":
		updateCommand = packageCommand{name: "sudo", args: []string{manager, "update"}}
		installCommand = packageCommand{name: "sudo", args: []string{manager, "install", "-y", packageName}}
	case "brew":
		installCommand = packageCommand{name: "brew", args: []string{"install", packageName}}
	case "apk":
		updateCommand = packageCommand{name: "sudo", args: []string{"apk", "update"}}
		installCommand = packageCommand{name: "sudo", args: []string{"apk", "add", packageName}}
	case "yum", "dnf":
		installCommand = packageCommand{name: "sudo", args: []string{manager, "install", "-y", packageName}}
	case "pacman":
		updateCommand = packageCommand{name: "sudo", args: []string{"pacman", "-Sy"}}
		installCommand = packageCommand{name: "sudo", args: []string{"pacman", "-S", "--noconfirm", packageName}}
	case "zypper":
		updateCommand = packageCommand{name: "sudo", args: []string{"zypper", "refresh"}}
		installCommand = packageCommand{name: "sudo", args: []string{"zypper", "install", "-y", packageName}}
	case "xbps-install":
		updateCommand = packageCommand{name: "sudo", args: []string{"xbps-install", "-S"}}
		installCommand = packageCommand{name: "sudo", args: []string{"xbps-install", "-y", packageName}}
	case "emerge":
		installCommand = packageCommand{name: "sudo", args: []string{"emerge", packageName}}
	case "nix-env":
		installCommand = packageCommand{name: "nix-env", args: []string{"-i", packageName}}
	case "guix":
		installCommand = packageCommand{name: "guix", args: []string{"install", packageName}}
	case "pkg":
		updateCommand = packageCommand{name: "sudo", args: []string{"pkg", "update"}}
		installCommand = packageCommand{name: "sudo", args: []string{"pkg", "install", "-y", packageName}}
	case "slackpkg":
		updateCommand = packageCommand{name: "sudo", args: []string{"slackpkg", "update"}}
		installCommand = packageCommand{name: "sudo", args: []string{"slackpkg", "install", packageName}}
	default:
		return packageCommand{}, packageCommand{}, fmt.Errorf("unsupported package manager: %s", manager)
	}
	return updateCommand, installCommand, nil
}

func validPackageName(name string) bool {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "-") {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '-', '_', '.', '+', ':', '/', '@', '=':
			continue
		default:
			return false
		}
	}
	return true
}
