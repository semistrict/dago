package dadev

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configures the local development supervisor.
type Options struct {
	ConfigPath string
	Host       string
	Port       int
	Workers    int
	Browser    bool
	Stdout     io.Writer
	Stderr     io.Writer
}

// Run builds a configured Agent Server, supervises it, and rebuilds it when Go
// source, module, configuration, or environment files change.
func Run(ctx context.Context, options Options) error {
	options = applyDefaults(options)
	configPath, err := filepath.Abs(options.ConfigPath)
	if err != nil {
		return err
	}
	directory := filepath.Dir(configPath)
	watchRoot, err := goModuleRoot(ctx, directory)
	if err != nil {
		return err
	}
	workDirectory := filepath.Join(directory, ".dago_api")
	mainPath := filepath.Join(workDirectory, "devmain", "main.go")
	binaryPath := filepath.Join(workDirectory, "server")
	address := options.Host + ":" + strconv.Itoa(options.Port)
	localURL := "http://" + address
	studioURL := "https://smith.langchain.com/studio?baseUrl=" + localURL

	build := func() ([]string, []string, error) {
		config, err := loadConfig(configPath)
		if err != nil {
			return nil, nil, err
		}
		environment, envWatch, err := loadEnvironment(config, directory)
		if err != nil {
			return nil, nil, err
		}
		graphs, err := resolveGraphs(ctx, config, directory)
		if err != nil {
			return nil, nil, err
		}
		if err := generateMain(graphs, mainPath); err != nil {
			return nil, nil, err
		}
		next := binaryPath + ".next"
		command := exec.CommandContext(ctx, "go", "build", "-o", next, "./.dago_api/devmain")
		command.Dir, command.Env, command.Stdout, command.Stderr = directory, environment, options.Stdout, options.Stderr
		if err := command.Run(); err != nil {
			return nil, nil, fmt.Errorf("build development server: %w", err)
		}
		if err := os.Rename(next, binaryPath); err != nil {
			return nil, nil, err
		}
		return append([]string{configPath}, envWatch...), environment, nil
	}

	watched, environment, err := build()
	if err != nil {
		return err
	}
	process, err := startProcess(binaryPath, directory, environment, options, address)
	if err != nil {
		return err
	}
	defer process.stop()
	if err := waitReady(ctx, process, localURL); err != nil {
		return err
	}
	fmt.Fprintf(options.Stdout, "API: %s\nStudio: %s\n", localURL, studioURL)
	if options.Browser && os.Getenv("BROWSER") != "none" {
		if err := openBrowser(studioURL); err != nil {
			fmt.Fprintf(options.Stderr, "open Studio: %v\n", err)
		}
	}
	signature := sourceSignature(watchRoot, watched)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-process.done:
			if ctx.Err() != nil {
				return nil
			}
			if err := process.waitError(); err != nil {
				return fmt.Errorf("development server exited: %w", err)
			}
			return fmt.Errorf("development server exited")
		case <-ticker.C:
			next := sourceSignature(watchRoot, watched)
			if next == signature {
				continue
			}
			newWatched, newEnvironment, err := build()
			if err != nil {
				fmt.Fprintf(options.Stderr, "rebuild failed: %v\n", err)
				signature = next
				continue
			}
			process.stop()
			process, err = startProcess(binaryPath, directory, newEnvironment, options, address)
			if err != nil {
				return err
			}
			if err := waitReady(ctx, process, localURL); err != nil {
				return err
			}
			watched = newWatched
			environment = newEnvironment
			signature = sourceSignature(watchRoot, watched)
			fmt.Fprintln(options.Stdout, "Reloaded.")
		}
	}
}

func goModuleRoot(ctx context.Context, directory string) (string, error) {
	command := exec.CommandContext(ctx, "go", "env", "GOMOD")
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("find Go module: %w: %s", err, strings.TrimSpace(string(output)))
	}
	moduleFile := strings.TrimSpace(string(output))
	if moduleFile == "" || moduleFile == os.DevNull {
		return "", fmt.Errorf("dago dev requires a Go module")
	}
	return filepath.Dir(moduleFile), nil
}

func applyDefaults(options Options) Options {
	if options.Host == "" {
		options.Host = "localhost"
	}
	if options.Port == 0 {
		options.Port = 2024
	}
	if options.Workers <= 0 {
		options.Workers = 10
	}
	if options.Stdout == nil {
		options.Stdout = os.Stdout
	}
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}
	return options
}

type childProcess struct {
	command *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func startProcess(path, directory string, environment []string, options Options, address string) (*childProcess, error) {
	command := exec.Command(path)
	command.Dir, command.Stdout, command.Stderr = directory, options.Stdout, options.Stderr
	command.Env = overlayEnvironment(environment, map[string]string{
		"DAGO_DEV_ADDRESS":   address,
		"DAGO_DEV_STATE_DIR": filepath.Join(directory, ".dago_api", "state"),
		"DAGO_DEV_WORKERS":   strconv.Itoa(options.Workers),
	})
	if err := command.Start(); err != nil {
		return nil, err
	}
	child := &childProcess{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		child.mu.Lock()
		child.err = err
		child.mu.Unlock()
		close(child.done)
	}()
	return child, nil
}

func (child *childProcess) waitError() error {
	child.mu.Lock()
	defer child.mu.Unlock()
	return child.err
}

func (child *childProcess) stop() {
	if child == nil || child.command == nil || child.command.Process == nil {
		return
	}
	_ = child.command.Process.Signal(os.Interrupt)
	select {
	case <-child.done:
	case <-time.After(3 * time.Second):
		_ = child.command.Process.Kill()
		<-child.done
	}
}

func waitReady(ctx context.Context, child *childProcess, baseURL string) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 300 * time.Millisecond}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-child.done:
			if err := child.waitError(); err != nil {
				return fmt.Errorf("development server exited during startup: %w", err)
			}
			return fmt.Errorf("development server exited during startup")
		case <-deadline.C:
			return fmt.Errorf("development server did not become ready at %s", baseURL)
		case <-ticker.C:
			response, err := client.Get(baseURL + "/ok")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

func sourceSignature(directory string, explicit []string) string {
	var builder strings.Builder
	_ = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != directory && (entry.Name() == ".git" || entry.Name() == ".dago_api" || entry.Name() == "node_modules" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" && entry.Name() != "go.mod" && entry.Name() != "go.sum" {
			return nil
		}
		appendFileSignature(&builder, path)
		return nil
	})
	for _, path := range explicit {
		appendFileSignature(&builder, path)
	}
	return builder.String()
}

func appendFileSignature(builder *strings.Builder, path string) {
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(builder, "%s:missing;", path)
		return
	}
	fmt.Fprintf(builder, "%s:%d:%d;", path, info.Size(), info.ModTime().UnixNano())
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}

func overlayEnvironment(base []string, values map[string]string) []string {
	merged := map[string]string{}
	for _, entry := range base {
		if key, value, ok := strings.Cut(entry, "="); ok {
			merged[key] = value
		}
	}
	for key, value := range values {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+merged[key])
	}
	return result
}
