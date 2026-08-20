package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/modelconfig"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/daproviders/openrouter"
)

func resolveConfiguredModelAuthentication(ctx context.Context, model, apiKey, stateDirectory string, stderr io.Writer, options modelconfig.ResolveOptions) (modelAuthentication, error) {
	authPath, err := authStorePath("")
	if err != nil {
		return modelAuthentication{}, err
	}
	credentials := dacredential.NewStore(authPath, time.Now, dacredential.Options{})
	parser := modelconfig.NewResolver(credentials, os.LookupEnv, map[string]modelconfig.Factory{}, modelconfig.Options{})
	spec, err := parser.Parse(ctx, model)
	if err != nil {
		return modelAuthentication{}, err
	}
	authentication := modelAuthentication{modelOptions: options}
	switch spec.Provider {
	case "openai":
		authentication, err = resolveAuthentication(ctx, apiKey, stateDirectory, stderr, defaultAuthenticationHooks())
		if err != nil {
			return modelAuthentication{}, err
		}
		authentication.modelOptions = options
		if authentication.subscription {
			return authentication, nil
		}
	case "openai_oauth":
		authentication, err = resolveOAuthAuthentication(ctx, stateDirectory, stderr, defaultAuthenticationHooks())
		if err != nil {
			return modelAuthentication{}, err
		}
		authentication.modelOptions = options
		return authentication, nil
	}
	authentication.resolver = modelconfig.NewResolver(credentials, os.LookupEnv, map[string]modelconfig.Factory{
		"openai":     openAIModelFactory,
		"openrouter": openRouterModelFactory,
	}, modelconfig.Options{})
	return authentication, nil
}

func resolveOAuthAuthentication(ctx context.Context, stateDirectory string, stderr io.Writer, hooks authenticationHooks) (modelAuthentication, error) {
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
	fmt.Fprintln(stderr, "No subscription sign-in found. Starting sign-in...")
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

func openAIModelFactory(_ context.Context, spec modelconfig.Spec, credential dacredential.Resolution, construction modelconfig.Construction) (damodel.Chat, error) {
	if credential.Credential.APIKey == nil {
		return nil, errors.New("openai factory requires an API key credential")
	}
	options, err := openAIDefaultConstructionOptions(construction)
	if err != nil {
		return nil, err
	}
	return openai.NewAPIKey(credential.Credential.APIKey.Key, spec.Model, options.clientOptions()), nil
}

func openRouterModelFactory(_ context.Context, spec modelconfig.Spec, credential dacredential.Resolution, construction modelconfig.Construction) (damodel.Chat, error) {
	if credential.Credential.APIKey == nil {
		return nil, errors.New("openrouter factory requires an API key credential")
	}
	parameters := cloneModelParameters(construction.Parameters)
	appURL, err := popStringParameter(parameters, "app_url")
	if err != nil {
		return nil, err
	}
	appTitle, err := popStringParameter(parameters, "app_title")
	if err != nil {
		return nil, err
	}
	var routing *openrouter.ProviderRouting
	if raw, exists := parameters["openrouter_provider"]; exists {
		payload, marshalErr := json.Marshal(raw)
		if marshalErr != nil {
			return nil, errors.New("openrouter provider routing is invalid")
		}
		routing = &openrouter.ProviderRouting{}
		if unmarshalErr := json.Unmarshal(payload, routing); unmarshalErr != nil {
			return nil, errors.New("openrouter provider routing is invalid")
		}
		delete(parameters, "openrouter_provider")
	}
	common, err := openAIConstructionOptions(modelconfig.Construction{
		BaseURL: construction.BaseURL, Parameters: parameters,
		MaxRetries: construction.MaxRetries, HasMaxRetries: construction.HasMaxRetries,
	})
	if err != nil {
		return nil, err
	}
	return openrouter.New(credential.Credential.APIKey.Key, spec.Model, openrouter.Options{
		BaseURL: common.BaseURL, MaxOutputTokens: common.MaxOutputTokens,
		ContextWindow: common.ContextWindow, WebSearch: common.WebSearch,
		RetryBackoff: common.RetryBackoff, AppURL: appURL, AppTitle: appTitle, Routing: routing,
	}), nil
}

type openAIOptions struct {
	BaseURL         string
	MaxOutputTokens int
	ContextWindow   int
	WebSearch       bool
	RetryBackoff    []time.Duration
}

func (options openAIOptions) clientOptions() openai.Options {
	return openai.Options{
		BaseURL: options.BaseURL, MaxOutputTokens: options.MaxOutputTokens,
		ContextWindow: options.ContextWindow, RetryBackoff: options.RetryBackoff,
		WebSearch: options.WebSearch,
	}
}

func openAIConstructionOptions(construction modelconfig.Construction) (openAIOptions, error) {
	parameters := cloneModelParameters(construction.Parameters)
	options := openAIOptions{BaseURL: construction.BaseURL, ContextWindow: 128_000}
	if value, exists := parameters["use_responses_api"]; exists {
		enabled, ok := value.(bool)
		if !ok || !enabled {
			return openAIOptions{}, errors.New("use_responses_api must be true")
		}
		delete(parameters, "use_responses_api")
	}
	if value, exists := parameters["context_window"]; exists {
		parsed, ok := modelInteger(value)
		if !ok || parsed < 1 {
			return openAIOptions{}, errors.New("context_window must be a positive integer")
		}
		options.ContextWindow = parsed
		delete(parameters, "context_window")
	}
	if value, exists := parameters["max_output_tokens"]; exists {
		parsed, ok := modelInteger(value)
		if !ok || parsed < 1 {
			return openAIOptions{}, errors.New("max_output_tokens must be a positive integer")
		}
		options.MaxOutputTokens = parsed
		delete(parameters, "max_output_tokens")
	}
	if value, exists := parameters["web_search"]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return openAIOptions{}, errors.New("web_search must be a boolean")
		}
		options.WebSearch = enabled
		delete(parameters, "web_search")
	}
	delete(parameters, "max_retries")
	if len(parameters) != 0 {
		names := make([]string, 0, len(parameters))
		for name := range parameters {
			names = append(names, name)
		}
		sort.Strings(names)
		return openAIOptions{}, fmt.Errorf("OpenAI adapter does not support model parameters: %s", strings.Join(names, ", "))
	}
	if construction.HasMaxRetries {
		options.RetryBackoff = retryBackoff(construction.MaxRetries)
	}
	return options, nil
}

func openAIDefaultConstructionOptions(construction modelconfig.Construction) (openAIOptions, error) {
	options, err := openAIConstructionOptions(construction)
	if err != nil {
		return openAIOptions{}, err
	}
	if _, configured := construction.Parameters["web_search"]; !configured {
		options.WebSearch = true
	}
	return options, nil
}

func applyLegacyModelOptions(chat damodel.Chat, options modelconfig.ResolveOptions) (damodel.Chat, error) {
	return modelconfig.ApplyProfile(chat, options.ProfileOverrides)
}

func legacyOpenAIOptions(baseURL string, options modelconfig.ResolveOptions) (openAIOptions, error) {
	construction := modelconfig.Construction{BaseURL: baseURL, Parameters: options.Parameters}
	if options.BaseURL != nil {
		construction.BaseURL = *options.BaseURL
	}
	if options.MaxRetries != nil {
		construction.MaxRetries, construction.HasMaxRetries = *options.MaxRetries, true
	}
	return openAIDefaultConstructionOptions(construction)
}

func retryBackoff(retries int) []time.Duration {
	if retries <= 0 {
		return []time.Duration{}
	}
	result := make([]time.Duration, retries)
	delay := time.Second
	for index := range result {
		result[index] = delay
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
	return result
}

func cloneModelParameters(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func modelInteger(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case float64:
		return int(value), value == float64(int(value))
	case json.Number:
		parsed, err := strconv.Atoi(string(value))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func popStringParameter(values map[string]any, name string) (string, error) {
	value, exists := values[name]
	if !exists {
		return "", nil
	}
	delete(values, name)
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return text, nil
}
