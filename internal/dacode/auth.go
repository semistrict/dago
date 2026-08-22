package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/modelconfig"
	"github.com/semistrict/dago/daproviders/openai"
)

const (
	oauthStoreFilename = "openai-oauth.json"
	oauthLoginTimeout  = 5 * time.Minute
)

type modelAuthentication struct {
	apiKey           string
	credentials      openai.CredentialSource
	subscription     bool
	pairedURL        string
	hasPairedURL     bool
	resolver         *modelconfig.Resolver
	workflowResolver *modelconfig.Resolver
	primaryProvider  string
	modelOptions     modelconfig.ResolveOptions
	decorateModel    func(damodel.Chat) damodel.Chat
}

func (authentication modelAuthentication) String() string {
	return fmt.Sprintf("modelAuthentication(subscription=%t,paired_url=%t,<redacted>)", authentication.subscription, authentication.hasPairedURL)
}

func (authentication modelAuthentication) GoString() string { return authentication.String() }

type authenticationHooks struct {
	loadStored      func(context.Context) (dacredential.APIKeyCredential, bool, error)
	load            func(string, openai.OAuthOptions) (*openai.OAuthSession, error)
	login           func(context.Context, func(string) error, openai.OAuthOptions) (*openai.OAuthSession, error)
	openURL         func(string) error
	legacyStorePath func() (string, error)
}

func defaultAuthenticationHooks() authenticationHooks {
	return authenticationHooks{
		loadStored: loadDefaultOpenAICredential,
		load:       openai.LoadOAuthSession, login: openai.Login, openURL: openExternalURL,
		legacyStorePath: legacyOpenAIOAuthStorePath,
	}
}

func resolveAuthentication(ctx context.Context, apiKey, stateDirectory string, stderr io.Writer, hooks authenticationHooks) (modelAuthentication, error) {
	return resolveAuthenticationWithOptions(ctx, apiKey, stateDirectory, stderr, hooks, authenticationResolveOptions{InteractiveLogin: true})
}

type authenticationResolveOptions struct {
	InteractiveLogin bool
}

var errACPAuthenticationRequired = errors.New("model authentication is not configured; run dacode auth or configure provider credentials before starting ACP")

func resolveAuthenticationWithOptions(ctx context.Context, apiKey, stateDirectory string, stderr io.Writer, hooks authenticationHooks, options authenticationResolveOptions) (modelAuthentication, error) {
	if hooks.loadStored != nil {
		stored, ok, err := hooks.loadStored(ctx)
		if err != nil {
			return modelAuthentication{}, fmt.Errorf("load stored OpenAI credential: %w", err)
		}
		if ok {
			return modelAuthentication{apiKey: stored.Key, pairedURL: stored.BaseURL, hasPairedURL: true}, nil
		}
	}
	if key := strings.TrimSpace(apiKey); key != "" {
		return modelAuthentication{apiKey: key}, nil
	}
	if stderr == nil {
		stderr = io.Discard
	}
	storePath := filepath.Join(stateDirectory, oauthStoreFilename)
	session, err := hooks.load(storePath, openai.OAuthOptions{})
	if err == nil {
		return modelAuthentication{credentials: session, subscription: true}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return modelAuthentication{}, fmt.Errorf("load saved OpenAI sign-in from %s: %w; remove the file to sign in again", storePath, err)
	}
	legacy, legacyErr := loadLegacyOAuthSession(hooks, storePath)
	if legacyErr != nil {
		return modelAuthentication{}, legacyErr
	}
	if legacy != nil {
		return modelAuthentication{credentials: legacy, subscription: true}, nil
	}
	if !options.InteractiveLogin {
		return modelAuthentication{}, errACPAuthenticationRequired
	}

	fmt.Fprintln(stderr, "No API key found. Starting OpenAI subscription sign-in...")
	loginCtx, cancel := context.WithTimeout(ctx, oauthLoginTimeout)
	defer cancel()
	session, err = hooks.login(loginCtx, func(value string) error {
		fmt.Fprintf(stderr, "Open this URL to sign in:\n%s\n", value)
		if hooks.openURL != nil {
			if openErr := hooks.openURL(value); openErr != nil {
				fmt.Fprintf(stderr, "Could not open a browser automatically: %v\n", openErr)
			}
		}
		return nil
	}, openai.OAuthOptions{StorePath: storePath})
	if err != nil {
		return modelAuthentication{}, fmt.Errorf("OpenAI subscription sign-in: %w", err)
	}
	fmt.Fprintln(stderr, "Sign-in complete.")
	return modelAuthentication{credentials: session, subscription: true}, nil
}

func legacyOpenAIOAuthStorePath() (string, error) {
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDirectory, ".deepagents", ".state", "chatgpt-auth.json"), nil
}

func loadLegacyOAuthSession(hooks authenticationHooks, primaryPath string) (*openai.OAuthSession, error) {
	if hooks.legacyStorePath == nil {
		return nil, nil
	}
	legacyPath, err := hooks.legacyStorePath()
	if err != nil {
		return nil, fmt.Errorf("resolve existing Deep Agents sign-in: %w", err)
	}
	if strings.TrimSpace(legacyPath) == "" || filepath.Clean(legacyPath) == filepath.Clean(primaryPath) {
		return nil, nil
	}
	session, err := hooks.load(legacyPath, openai.OAuthOptions{})
	if err == nil {
		return session, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return nil, fmt.Errorf("load existing Deep Agents sign-in from %s: %w", legacyPath, err)
}

func loadDefaultOpenAICredential(ctx context.Context) (dacredential.APIKeyCredential, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return dacredential.APIKeyCredential{}, false, nil
	}
	return loadOpenAICredential(ctx, dacredential.DefaultPath(home))
}

func loadOpenAICredential(ctx context.Context, path string) (dacredential.APIKeyCredential, bool, error) {
	store := dacredential.NewStore(path, time.Now, dacredential.Options{})
	snapshot, err := store.Load(ctx)
	if err != nil {
		return dacredential.APIKeyCredential{}, false, err
	}
	credential, ok := snapshot.APIKey("openai")
	return credential, ok, nil
}

func (authentication modelAuthentication) newModel(model, baseURL string) (damodel.Chat, error) {
	return authentication.resolveModel(context.Background(), model, baseURL)
}

func (authentication modelAuthentication) resolveModel(ctx context.Context, model, baseURL string) (damodel.Chat, error) {
	if provider, _, explicit := strings.Cut(model, ":"); explicit && provider != "openai" && provider != "openai_oauth" && authentication.workflowResolver != nil {
		resolution, err := authentication.workflowResolver.Resolve(ctx, model, modelconfig.ResolveOptions{})
		if err != nil {
			return nil, err
		}
		return authentication.decorate(resolution.Model), nil
	}
	if authentication.resolver != nil {
		options := authentication.modelOptions
		if provider, _, explicit := strings.Cut(model, ":"); explicit && authentication.primaryProvider != "" && !strings.EqualFold(provider, authentication.primaryProvider) {
			options = modelconfig.ResolveOptions{}
		}
		if baseURL != "" && options.BaseURL == nil {
			options.BaseURL = new(baseURL)
		}
		resolution, err := authentication.resolver.Resolve(ctx, model, options)
		if err != nil {
			return nil, err
		}
		return authentication.decorate(resolution.Model), nil
	}
	if provider, identifier, found := strings.Cut(model, ":"); found && (provider == "openai" || provider == "openai_oauth") {
		model = identifier
	}
	configuredBaseURL := baseURL
	if authentication.hasPairedURL {
		configuredBaseURL = authentication.pairedURL
	}
	options, err := legacyOpenAIOptions(configuredBaseURL, authentication.modelOptions)
	if err != nil {
		return nil, err
	}
	if authentication.subscription {
		options.ContextWindow = 272_000
		chat := openai.NewSubscription(authentication.credentials, model, options.clientOptions())
		configured, err := applyLegacyModelOptions(chat, authentication.modelOptions)
		if err != nil {
			return nil, err
		}
		return authentication.decorate(configured), nil
	}
	if strings.TrimSpace(authentication.apiKey) == "" {
		return nil, errors.New("OpenAI API key is required")
	}
	chat := openai.NewAPIKey(authentication.apiKey, model, options.clientOptions())
	configured, err := applyLegacyModelOptions(chat, authentication.modelOptions)
	if err != nil {
		return nil, err
	}
	return authentication.decorate(configured), nil
}

func (authentication modelAuthentication) decorate(model damodel.Chat) damodel.Chat {
	if authentication.decorateModel != nil {
		return authentication.decorateModel(model)
	}
	return model
}

func openExternalURL(target string) error {
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
