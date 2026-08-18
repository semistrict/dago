package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/semistrict/dago/damanaged"
	"github.com/semistrict/dago/internal/dacli"
	"github.com/semistrict/dago/internal/dadev"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	return runWithIO(arguments, os.Stdin, os.Stdout, os.Stderr)
}

func runWithIO(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("interactive chat has moved to dacode; run `dacode` to start it (dago now provides dev, init, deploy, agents, and mcp-servers)")
	}
	switch arguments[0] {
	case "dev":
		return runDev(arguments[1:], stdout, stderr)
	case "init":
		return runInit(arguments[1:], stdin, stdout, stderr)
	case "agents":
		return runAgents(arguments[1:], stdin, stdout, stderr)
	case "deploy":
		return runDeploy(arguments[1:], stdin, stdout, stderr)
	case "mcp-servers":
		return runMCPServers(arguments[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q; usage: dago dev | init | deploy | agents | mcp-servers", arguments[0])
	}
}

type managedDeploymentClient interface {
	dacli.ManagedDeployer
	GetAgent(context.Context, string, bool) (damanaged.Agent, error)
	GetAgentHealth(context.Context, string) (any, error)
}

func runDeploy(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("dago deploy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	directory := flags.String("dir", ".", "managed-agent project directory")
	dryRun := flags.Bool("dry-run", false, "print the deployment payload without sending")
	detach := flags.Bool("detach", false, "skip the post-deploy health request")
	reset := flags.Bool("reset", false, "forget the locally pinned remote agent and create a new one")
	yes := flags.Bool("yes", false, "confirm a declared remote target without prompting")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: dago deploy [--dir PATH] [--dry-run] [--detach] [--reset] [--yes]")
	}
	project, err := damanaged.LoadProject(*directory)
	if err != nil {
		return err
	}
	if *dryRun {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(dacli.DryRunPayload(project))
	}
	client, err := managedClientFromEnvironment()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user state directory: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDeployWithClient(ctx, client, project, filepath.Join(home, ".deepagents", "deployments"), dacli.DeployOptions{Reset: *reset}, *yes, *detach, stdin, stdout)
}

func runDeployWithClient(ctx context.Context, client managedDeploymentClient, project damanaged.Project, stateRoot string, options dacli.DeployOptions, assumeYes, detach bool, stdin io.Reader, stdout io.Writer) error {
	if client == nil {
		panic("managed deploy command client is required")
	}
	if project.AgentID != "" && !assumeYes {
		agent, err := client.GetAgent(ctx, project.AgentID, false)
		if err != nil {
			return err
		}
		name, _ := agent["name"].(string)
		if _, err := fmt.Fprintf(stdout, "Deploy to agent %s (%s) from agent.json? [y/N]: ", name, project.AgentID); err != nil {
			return err
		}
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 64), 1024)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read deploy confirmation: %w", err)
			}
			return errors.New("deployment aborted")
		}
		answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if answer != "y" && answer != "yes" {
			return errors.New("deployment aborted")
		}
	}
	result, err := dacli.Deploy(ctx, client, project, stateRoot, options)
	if err != nil {
		return err
	}
	endpoint := strings.Replace(client.Endpoint(), "api.smith.langchain.com", "smith.langchain.com", 1)
	if _, err := fmt.Fprintf(stdout, "\nDeployed: %s\n  agent_id: %s\n  revision: %.8s\n  %s/o/-/agents/%s\n", safeCLIValue(result.Name), safeCLIValue(result.AgentID), safeCLIValue(result.Revision), strings.TrimRight(endpoint, "/"), result.AgentID); err != nil {
		return err
	}
	if !detach {
		health, healthErr := client.GetAgentHealth(ctx, result.AgentID)
		if healthErr != nil {
			_, _ = fmt.Fprintf(stdout, "  health check skipped: %v\n", healthErr)
		} else {
			encoded, _ := json.Marshal(health)
			_, _ = fmt.Fprintf(stdout, "  health: %s\n", encoded)
		}
	}
	return nil
}

type managedAgents interface {
	ListAgents(context.Context, string) ([]damanaged.Agent, error)
	GetAgent(context.Context, string, bool) (damanaged.Agent, error)
	DeleteAgent(context.Context, string) error
}

func runAgents(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	client, err := managedClientFromEnvironment()
	if err != nil {
		return err
	}
	return runAgentsWithClient(context.Background(), client, arguments, stdin, stdout, stderr)
}

func runAgentsWithClient(ctx context.Context, client managedAgents, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if client == nil {
		panic("managed-agent command client is required")
	}
	if len(arguments) == 0 {
		return fmt.Errorf("usage: dago agents list | get [--include-files] ID | delete [--yes] ID")
	}
	switch arguments[0] {
	case "list":
		flags := flag.NewFlagSet("dago agents list", flag.ContinueOnError)
		flags.SetOutput(stderr)
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: dago agents list")
		}
		agents, err := client.ListAgents(ctx, "")
		if err != nil {
			return err
		}
		for _, agent := range agents {
			if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", safeCLIValue(agent["id"]), safeCLIValue(agent["name"]), safeCLIValue(agent["updated_at"])); err != nil {
				return err
			}
		}
		return nil
	case "get":
		flags := flag.NewFlagSet("dago agents get", flag.ContinueOnError)
		flags.SetOutput(stderr)
		includeFiles := flags.Bool("include-files", false, "include the agent directory projection")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: dago agents get [--include-files] ID")
		}
		agent, err := client.GetAgent(ctx, flags.Arg(0), *includeFiles)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(agent)
	case "delete":
		flags := flag.NewFlagSet("dago agents delete", flag.ContinueOnError)
		flags.SetOutput(stderr)
		yes := flags.Bool("yes", false, "delete without an interactive confirmation")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: dago agents delete [--yes] ID")
		}
		agentID := flags.Arg(0)
		if !*yes {
			if _, err := fmt.Fprintf(stdout, "Delete agent %s? [y/N]: ", agentID); err != nil {
				return err
			}
			scanner := bufio.NewScanner(stdin)
			scanner.Buffer(make([]byte, 64), 1024)
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("read delete confirmation: %w", err)
				}
				_, _ = fmt.Fprintln(stdout, "Aborted.")
				return nil
			}
			answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
			if answer != "y" && answer != "yes" {
				_, _ = fmt.Fprintln(stdout, "Aborted.")
				return nil
			}
		}
		if err := client.DeleteAgent(ctx, agentID); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "Deleted %s\n", agentID)
		return err
	default:
		return fmt.Errorf("unknown agents command %q", arguments[0])
	}
}

func managedClientFromEnvironment() (*damanaged.Client, error) {
	apiKey := strings.TrimSpace(os.Getenv("LANGSMITH_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("LANGCHAIN_API_KEY"))
	}
	if apiKey == "" || len(apiKey) > 16<<10 || strings.ContainsAny(apiKey, "\x00\r\n") {
		return nil, fmt.Errorf("set LANGSMITH_API_KEY or LANGCHAIN_API_KEY")
	}
	endpoint := strings.TrimSpace(os.Getenv("LANGSMITH_ENDPOINT"))
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("LANGCHAIN_ENDPOINT"))
	}
	if endpoint == "" {
		endpoint = "https://api.smith.langchain.com"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("LANGSMITH_ENDPOINT or LANGCHAIN_ENDPOINT must be an HTTPS origin without credentials, query, or fragment")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	httpClient := &http.Client{Transport: transport, Timeout: 35 * time.Second}
	return damanaged.New(httpClient, endpoint, apiKey, damanaged.Options{}), nil
}

func runDev(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("dago dev", flag.ContinueOnError)
	flags.SetOutput(stderr)
	config := flags.String("config", "dago.json", "path to dago.json")
	flags.StringVar(config, "c", "dago.json", "path to dago.json")
	host := flags.String("host", "localhost", "host to bind")
	port := flags.Int("port", 2024, "port to bind")
	flags.IntVar(port, "p", 2024, "port to bind")
	workers := flags.Int("n-jobs-per-worker", 10, "concurrent run workers")
	noBrowser := flags.Bool("no-browser", false, "do not open Studio")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dadev.Run(ctx, dadev.Options{
		ConfigPath: *config, Host: *host, Port: *port, Workers: *workers,
		Browser: !*noBrowser, Stdout: stdout, Stderr: stderr,
	})
}

func runInit(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("dago init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	force := flags.Bool("force", false, "overwrite starter files in an existing project")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("usage: dago init [--force] [name]")
	}
	name := ""
	if flags.NArg() == 1 {
		name = flags.Arg(0)
	} else {
		if _, err := fmt.Fprint(stdout, "Project name: "); err != nil {
			return fmt.Errorf("write project prompt: %w", err)
		}
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 256), 4096)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read project name: %w", err)
			}
			return fmt.Errorf("project name is required")
		}
		name = strings.TrimSpace(scanner.Text())
		if name == "" {
			return fmt.Errorf("project name is required")
		}
	}
	project, err := dacli.Scaffold(".", name, dacli.ScaffoldOptions{Force: *force})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, `Created %s with agent.json, AGENTS.md, .gitignore, tools.json, an example skill, and a researcher subagent.

Next steps
  1. Edit AGENTS.md, agent.json, and tools.json.
  2. Replace or remove the examples under skills/ and subagents/.
  3. Run the agent deployment command when its credentials are configured.
`, project)
	return err
}
