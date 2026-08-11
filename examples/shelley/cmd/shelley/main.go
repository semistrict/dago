package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/semistrict/dago/examples/shelley/claudetool"
	"github.com/semistrict/dago/examples/shelley/client"
	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/llm/llmhttp"
	"github.com/semistrict/dago/examples/shelley/models"
	"github.com/semistrict/dago/examples/shelley/modelsources"
	"github.com/semistrict/dago/examples/shelley/server"
	_ "github.com/semistrict/dago/examples/shelley/server/notifications/channels" // register channel types
	"github.com/semistrict/dago/examples/shelley/skills"
	"github.com/semistrict/dago/examples/shelley/version"
)

type GlobalConfig struct {
	DBPath           string
	Debug            bool
	PredictableOnly  bool
	ConfigPath       string
	DefaultModel     string
	OpenAIOAuthStore string
}

type shelleyConfig struct {
	DefaultModel string `json:"default_model"`
}

// registerGlobalFlags binds the process-wide global flags onto fs, writing into
// global. Extracted from main so tests can parse flags through a fresh FlagSet
// and assert defaults (notably that -default-model defaults to empty, which is
// what lets shelley.json's default_model take effect on VMs).
func registerGlobalFlags(fs *flag.FlagSet, global *GlobalConfig) {
	fs.StringVar(&global.DBPath, "db", "shelley.db", "Path to SQLite database file")
	fs.BoolVar(&global.Debug, "debug", false, "Enable debug logging")
	fs.BoolVar(&global.PredictableOnly, "predictable-only", false, "Use only the predictable service, ignoring all other models")
	fs.StringVar(&global.ConfigPath, "config", "", "Path to shelley.json configuration file (optional)")
	fs.StringVar(&global.DefaultModel, "default-model", "", "Default model for web UI (overrides shelley.json default_model; falls back to the first ready model when unset)")
	fs.StringVar(&global.OpenAIOAuthStore, "openai-oauth-store", "", "OpenAI subscription OAuth token file (defaults to the user config directory; set to 'none' to disable)")
}

func main() {
	// Define global flags
	var global GlobalConfig
	registerGlobalFlags(flag.CommandLine, &global)

	// Custom usage function
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [global-flags] <command> [command-flags]\n\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "Global flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "\nCommands:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  serve [flags]                 Start the web server\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  models [flags]                List the models the server would expose, without starting it\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  client [flags] <subcommand>   CLI client (chat, read, list, archive) (experimental)\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  skill <cat|ls|new> [name]     Read, list, or create skills\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  dtach <new|attach> ...        Persistent PTY sessions over a Unix socket\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  version                       Print version information as JSON\n")
		fmt.Fprintf(flag.CommandLine.Output(), "\nUse '%s <command> -h' for command-specific help\n", os.Args[0])
	}

	// Parse all flags first
	flag.Parse()
	args := flag.Args()

	if len(args) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	command := args[0]
	switch command {
	case "serve":
		runServe(global, args[1:])
	case "models":
		runModels(global, args[1:])
	case "client":
		client.Run(args[1:])
	case "skill":
		runSkill(args[1:])
	case "dtach":
		runDtach(args[1:])
	case "version":
		runVersion()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		flag.Usage()
		os.Exit(1)
	}
}

func runSkill(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: shelley skill <cat|cat-trusted|ls|new> [name]\n")
		os.Exit(1)
	}

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	switch args[0] {
	case "cat":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: shelley skill cat SKILL_NAME\n")
			os.Exit(1)
		}
		content, err := skills.FindByName(args[1], wd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(content)

	case "cat-trusted":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: shelley skill cat-trusted SKILL_NAME\n")
			os.Exit(1)
		}
		content, err := skills.FindTrustedByName(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(content)

	case "ls":
		all := skills.ListAll(wd, "")
		for _, s := range all {
			fmt.Printf("%s\t%s\n", s.Name, s.Description)
		}

	case "new":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: shelley skill new SKILL_NAME\n")
			os.Exit(1)
		}
		path, err := skills.CreateTemplate(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(path)

	default:
		fmt.Fprintf(os.Stderr, "Unknown skill subcommand: %s\nUsage: shelley skill <cat|cat-trusted|ls|new> [name]\n", args[0])
		os.Exit(1)
	}
}

func runServe(global GlobalConfig, args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "9000", "Port to listen on")
	listenHost := fs.String("listen-host", "127.0.0.1", "Host or IP address to listen on")
	portFile := fs.String("port-file", "", "Write the actual listening port to this file (useful with --port 0)")
	systemdActivation := fs.Bool("systemd-activation", false, "Use systemd socket activation (listen on fd from systemd)")
	requireHeader := fs.String("require-header", "", "Require this header on all API requests (e.g., X-User-ID)")
	trustWorkspaceGuidance := fs.Bool("trust-workspace-guidance", false, "Allow reviewed repository guidance and skills to influence the agent")
	socketPath := fs.String("socket", client.DefaultSocketPath(), "Path to Unix socket for local CLI client access (set to 'none' to disable)")
	banner := fs.String("banner", "", "If set, shows this text in a banner at the top of the UI (useful for marking demo instances)")
	fs.Parse(args)

	logger := setupLogging(global.Debug)
	if err := validateListenHost(*listenHost); err != nil {
		logger.Error("Refusing unsafe listener", "error", err)
		os.Exit(1)
	}

	database := setupDatabase(global.DBPath, logger)
	defer database.Close()
	openAIOAuth, err := setupOpenAIOAuth(global, logger)
	if err != nil {
		logger.Error("Failed to configure OpenAI subscription sign-in", "error", err)
		os.Exit(1)
	}
	if openAIOAuth != nil {
		defer openAIOAuth.Close()
	}

	// Set the database path for system prompt generation
	server.DBPath = global.DBPath

	// Build LLM configuration
	llmConfig, err := buildLLMConfigWithOAuth(global, logger, database, openAIOAuth)
	if err != nil {
		logger.Error("Failed to load config", "path", global.ConfigPath, "error", err)
		os.Exit(1)
	}

	// Initialize LLM service manager (includes custom model support via database)
	llmManager := server.NewLLMServiceManager(llmConfig)

	// Log available models
	availableModels := llmManager.GetAvailableModels()
	logger.Info("Available models", "models", strings.Join(availableModels, ", "))

	toolSetConfig := setupToolSetConfig(llmManager, llmManager)
	toolSetConfig.TrustWorkspaceGuidance = *trustWorkspaceGuidance

	// Create server
	svr := server.NewServer(database, llmManager, toolSetConfig, logger, global.PredictableOnly, llmConfig.DefaultModel, *requireHeader)
	svr.SetModelRefresher(llmConfig.RefreshBuiltModels)
	svr.SetOpenAIOAuth(openAIOAuth)
	svr.Banner = *banner
	svr.Debug = global.Debug

	// Load notification channels from DB.
	svr.ReloadNotificationChannels()

	// Resolve socket path: "none" disables the Unix socket listener
	effectiveSocket := *socketPath
	if effectiveSocket == "none" {
		effectiveSocket = ""
	}

	if *systemdActivation {
		listener, listenerErr := systemdListener()
		if listenerErr != nil {
			logger.Error("Failed to get systemd listener", "error", listenerErr)
			os.Exit(1)
		}
		tcpAddress, ok := listener.Addr().(*net.TCPAddr)
		if !ok || validateListenHost(tcpAddress.IP.String()) != nil {
			listener.Close()
			logger.Error("Refusing non-loopback systemd listener", "address", listener.Addr().String())
			os.Exit(1)
		}
		logger.Info("Using systemd socket activation")
		err = svr.StartWithListeners(listener, effectiveSocket)
	} else {
		listener, listenerErr := net.Listen("tcp", net.JoinHostPort(*listenHost, *port))
		if listenerErr != nil {
			logger.Error("Failed to create listener", "error", listenerErr)
			os.Exit(1)
		}
		if *portFile != "" {
			actualPort := listener.Addr().(*net.TCPAddr).Port
			if writeErr := os.WriteFile(*portFile, []byte(fmt.Sprintf("%d\n", actualPort)), 0o644); writeErr != nil {
				logger.Error("Failed to write port file", "path", *portFile, "error", writeErr)
				os.Exit(1)
			}
		}
		err = svr.StartWithListeners(listener, effectiveSocket)
	}

	if err != nil {
		logger.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

func validateListenHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("listen host is required")
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("non-loopback host %q is not supported; use a loopback listener behind an authenticated proxy", host)
}

func setupLogging(debug bool) *slog.Logger {
	logLevel := slog.LevelInfo
	if debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)
	return logger
}

func setupDatabase(dbPath string, logger *slog.Logger) *db.DB {
	database, err := db.New(db.Config{DSN: dbPath})
	if err != nil {
		logger.Error("Failed to initialize database", "error", err)
		os.Exit(1)
	}

	// Run database migrations
	if err := database.Migrate(context.Background()); err != nil {
		logger.Error("Failed to run database migrations", "error", err)
		os.Exit(1)
	}
	logger.Debug("Database migrations completed successfully")

	// Truncate the WAL at startup. The -wal file can grow large during
	// normal operation and a PASSIVE auto-checkpoint never shrinks it.
	if err := database.Checkpoint(context.Background()); err != nil {
		logger.Warn("Failed to checkpoint WAL at startup", "error", err)
	}

	// agent_working is runtime-only state. If the previous process exited
	// while a loop was running, the column can be left TRUE for one or more
	// conversations. Clear them so the conversation list reflects reality.
	if err := database.ResetAllAgentWorking(context.Background()); err != nil {
		logger.Error("Failed to reset agent_working state", "error", err)
		os.Exit(1)
	}
	return database
}

// runVersion prints version information as JSON
func runVersion() {
	info := version.GetInfo()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(info); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding version: %v\n", err)
		os.Exit(1)
	}
}

func setupToolSetConfig(llmProvider claudetool.LLMServiceProvider, llmManager server.LLMProvider) claudetool.ToolSetConfig {
	wd, err := os.Getwd()
	if err != nil {
		// Fallback to "/" if we can't get working directory
		wd = "/"
	}

	// Resolve the list of available models lazily, each time a ToolSet is
	// built. This lets newly-added custom models become visible to subagents
	// (and llm_one_shot) without restarting the server. See issue #195.
	buildAvailableModels := func() []claudetool.AvailableModel {
		availableIDs := llmManager.GetAvailableModels()
		tiers := models.AssignTiers(availableIDs)
		var out []claudetool.AvailableModel
		for _, id := range availableIDs {
			info := llmManager.GetModelInfo(id)
			// Only surface tier-1 models to agents; tier-2 models are
			// overshadowed by a better available sibling (see
			// models.AssignTiers) or are unknown integration models and would
			// just clutter the model enum. Explicit custom models stay visible.
			if tiers[id] == models.Tier2 && (info == nil || info.Source != models.SourceCustomLabel) {
				continue
			}
			am := claudetool.AvailableModel{ID: id}
			if info != nil && info.DisplayName != "" && info.DisplayName != id {
				am.DisplayName = info.DisplayName
			}
			out = append(out, am)
		}
		return out
	}

	return claudetool.ToolSetConfig{
		WorkingDir:           wd,
		LLMProvider:          llmProvider,
		EnableJITInstall:     claudetool.NoBashToolJITInstall,
		EnableBrowser:        true,
		BuildAvailableModels: buildAvailableModels,
	}
}

// buildLLMConfig composes models from local provider credentials, OpenAI
// account authentication, and the deterministic test model.
func buildLLMConfigWithOAuth(global GlobalConfig, logger *slog.Logger, database *db.DB, openAIOAuth *server.OpenAIOAuth) (*server.LLMConfig, error) {
	config, err := loadConfig(global.ConfigPath)
	if err != nil {
		return nil, err
	}
	defaultModel, sources := buildLLMModelSources(global, config, logger)

	httpc := llmhttp.NewClient(nil)
	builtModels, err := buildModels(modelsources.Build(models.All(), sources, httpc, logger), openAIOAuth)
	if err != nil {
		return nil, err
	}
	if defaultModel == "" && len(builtModels) > 0 && builtModels[0].ID == server.OpenAISubscriptionModelID {
		defaultModel = server.OpenAISubscriptionModelID
	}
	return &server.LLMConfig{
		Models:       builtModels,
		DefaultModel: defaultModel,
		DB:           database,
		HTTPC:        httpc,
		RefreshBuiltModels: func(_ context.Context) ([]models.Built, error) {
			_, sources := buildLLMModelSources(global, config, logger)
			return buildModels(modelsources.Build(models.All(), sources, httpc, logger), openAIOAuth)
		},
		Logger: logger,
	}, nil
}

func buildModels(base []models.Built, openAIOAuth *server.OpenAIOAuth) ([]models.Built, error) {
	subscription, err := openAIOAuth.BuiltModels()
	if err != nil {
		return nil, err
	}
	if len(subscription) == 0 {
		return base, nil
	}
	result := append([]models.Built(nil), subscription...)
	for _, built := range base {
		if built.ID != server.OpenAISubscriptionModelID {
			result = append(result, built)
		}
	}
	return result, nil
}

func setupOpenAIOAuth(global GlobalConfig, logger *slog.Logger) (*server.OpenAIOAuth, error) {
	storePath := strings.TrimSpace(global.OpenAIOAuthStore)
	if storePath == "none" {
		return nil, nil
	}
	if storePath == "" {
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user config directory: %w", err)
		}
		storePath = filepath.Join(configDirectory, "shelley", "openai-oauth.json")
	}
	return server.NewOpenAIOAuth(storePath, logger), nil
}

func loadConfig(path string) (shelleyConfig, error) {
	if path == "" {
		return shelleyConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return shelleyConfig{}, nil
		}
		return shelleyConfig{}, fmt.Errorf("read config file: %w", err)
	}
	var config shelleyConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return shelleyConfig{}, fmt.Errorf("parse config file: %w", err)
	}
	return config, nil
}

func buildLLMModelSources(global GlobalConfig, config shelleyConfig, logger *slog.Logger) (string, []modelsources.Source) {
	defaultModel := global.DefaultModel
	openAIKey := os.Getenv("OPENAI_API_KEY")
	var sources []modelsources.Source
	if config.DefaultModel != "" && defaultModel == "" {
		defaultModel = config.DefaultModel
		logger.Info("Using default model from config", "model", config.DefaultModel)
	}
	if openAIKey != "" {
		sources = append(sources, modelsources.Env(openAIKey))
	}
	sources = append(sources, modelsources.Predictable())
	return defaultModel, sources
}

func modelsCommandDefaultID(configured string, modelList []models.Built, predictableOnly bool) string {
	visible := func(model models.Built) bool {
		return (model.ID == "predictable") == predictableOnly
	}
	for _, model := range modelList {
		if visible(model) && model.ID == configured {
			return model.ID
		}
	}
	for _, model := range modelList {
		if visible(model) {
			return model.ID
		}
	}
	return ""
}

// runModels prints the materialized list of built-in models the server
// would expose, without starting the server. Useful for confirming that
// integrations/gateway/env-var precedence and discovery are configured
// correctly. Does NOT include custom (DB-backed) models.
func runModels(global GlobalConfig, args []string) {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: shelley [global-flags] models\n\n")
		fmt.Fprintf(fs.Output(), "Prints the built-in models Shelley would expose with the current\n")
		fmt.Fprintf(fs.Output(), "configuration (--config, env vars), without starting the server.\n")
	}
	fs.Parse(args)
	if fs.NArg() > 0 {
		fs.Usage()
		os.Exit(2)
	}

	logger := setupLogging(global.Debug)
	openAIOAuth, err := setupOpenAIOAuth(global, logger)
	if err != nil {
		logger.Error("Failed to configure OpenAI subscription sign-in", "error", err)
		os.Exit(1)
	}
	if openAIOAuth != nil {
		defer openAIOAuth.Close()
	}
	llmCfg, err := buildLLMConfigWithOAuth(global, logger, nil, openAIOAuth)
	if err != nil {
		logger.Error("Failed to load config", "path", global.ConfigPath, "error", err)
		os.Exit(1)
	}

	defaultID := modelsCommandDefaultID(llmCfg.DefaultModel, llmCfg.Models, global.PredictableOnly)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROVIDER\tAPI TYPE\tBASE URL\tSOURCE\tDEFAULT")
	for _, m := range llmCfg.Models {
		mark := ""
		if m.ID == defaultID {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Provider, m.APIType, m.BaseURL, m.Source, mark)
	}
	tw.Flush()
	fmt.Printf("\n%d models\n", len(llmCfg.Models))
}

// systemdListener returns a net.Listener from systemd socket activation.
// Systemd passes file descriptors starting at fd 3, with LISTEN_FDS indicating the count.
func systemdListener() (net.Listener, error) {
	// Check LISTEN_PID matches our PID (optional but recommended)
	pidStr := os.Getenv("LISTEN_PID")
	if pidStr != "" {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return nil, fmt.Errorf("invalid LISTEN_PID: %w", err)
		}
		if pid != os.Getpid() {
			return nil, fmt.Errorf("LISTEN_PID %d does not match current PID %d", pid, os.Getpid())
		}
	}

	// Get the number of file descriptors passed
	fdsStr := os.Getenv("LISTEN_FDS")
	if fdsStr == "" {
		return nil, fmt.Errorf("LISTEN_FDS not set; not running under systemd socket activation")
	}
	nfds, err := strconv.Atoi(fdsStr)
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_FDS: %w", err)
	}
	if nfds < 1 {
		return nil, fmt.Errorf("LISTEN_FDS=%d; expected at least 1", nfds)
	}

	// Systemd passes file descriptors starting at fd 3
	const listenFDsStart = 3
	fd := listenFDsStart

	// Create a file from the descriptor
	f := os.NewFile(uintptr(fd), "systemd-socket")
	if f == nil {
		return nil, fmt.Errorf("failed to create file from fd %d", fd)
	}

	// Create a listener from the file
	listener, err := net.FileListener(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("failed to create listener from fd %d: %w", fd, err)
	}

	// Close the original file; the listener now owns the descriptor
	f.Close()

	return listener, nil
}
