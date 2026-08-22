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
	"github.com/semistrict/dago/daproviders/anthropic"
	"github.com/semistrict/dago/daproviders/claudeagent"
	"github.com/semistrict/dago/daproviders/modelconfig"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/daproviders/openrouter"
)

func resolveConfiguredModelAuthentication(ctx context.Context, model, apiKey, stateDirectory string, stderr io.Writer, options modelconfig.ResolveOptions) (modelAuthentication, error) {
	return resolveConfiguredModelAuthenticationWithOptions(ctx, model, apiKey, stateDirectory, stderr, options, authenticationResolveOptions{InteractiveLogin: true})
}

func resolveConfiguredModelAuthenticationForACP(ctx context.Context, model, apiKey, stateDirectory string, stderr io.Writer, options modelconfig.ResolveOptions) (modelAuthentication, error) {
	return resolveConfiguredModelAuthenticationWithOptions(ctx, model, apiKey, stateDirectory, stderr, options, authenticationResolveOptions{})
}

func resolveConfiguredModelAuthenticationWithOptions(ctx context.Context, model, apiKey, stateDirectory string, stderr io.Writer, options modelconfig.ResolveOptions, authenticationOptions authenticationResolveOptions) (modelAuthentication, error) {
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
		authentication, err = resolveAuthenticationWithOptions(ctx, apiKey, stateDirectory, stderr, defaultAuthenticationHooks(), authenticationOptions)
		if err != nil {
			return modelAuthentication{}, err
		}
		authentication.modelOptions = options
	case "openai_oauth":
		authentication, err = resolveOAuthAuthenticationWithOptions(ctx, stateDirectory, stderr, defaultAuthenticationHooks(), authenticationOptions)
		if err != nil {
			return modelAuthentication{}, err
		}
	}
	resolver := modelconfig.NewResolver(credentials, os.LookupEnv, configuredModelFactories(), modelconfig.Options{})
	authentication.primaryProvider = spec.Provider
	if spec.Provider == "openai" || spec.Provider == "openai_oauth" {
		authentication.modelOptions = options
		authentication.workflowResolver = resolver
		return authentication, nil
	}
	authentication.resolver = resolver
	return authentication, nil
}

func configuredModelFactories() map[string]modelconfig.Factory {
	return map[string]modelconfig.Factory{
		"anthropic":    anthropicModelFactory,
		"claude_agent": claudeAgentModelFactory,
		"openai":       openAIModelFactory,
		"openrouter":   openRouterModelFactory,
	}
}

func anthropicModelFactory(_ context.Context, spec modelconfig.Spec, credential dacredential.Resolution, construction modelconfig.Construction) (damodel.Chat, error) {
	if credential.Credential.APIKey == nil {
		return nil, errors.New("anthropic factory requires an API key credential")
	}
	parameters := cloneModelParameters(construction.Parameters)
	options := anthropic.Options{BaseURL: anthropicMessagesEndpoint(construction.BaseURL), WebSearch: true}
	if value, exists := parameters["context_window"]; exists {
		parsed, ok := modelInteger(value)
		if !ok || parsed < 1 {
			return nil, errors.New("context_window must be a positive integer")
		}
		options.ContextWindow = parsed
		delete(parameters, "context_window")
	}
	if value, exists := parameters["max_output_tokens"]; exists {
		parsed, ok := modelInteger(value)
		if !ok || parsed < 1 {
			return nil, errors.New("max_output_tokens must be a positive integer")
		}
		options.MaxOutputTokens = parsed
		delete(parameters, "max_output_tokens")
	}
	if value, exists := parameters["web_search"]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return nil, errors.New("web_search must be a boolean")
		}
		options.WebSearch = enabled
		delete(parameters, "web_search")
	}
	var err error
	if options.HostedTools, err = popRawArrayParameter(parameters, "hosted_tools"); err != nil {
		return nil, err
	}
	if options.MCPServers, err = popRawArrayParameter(parameters, "mcp_servers"); err != nil {
		return nil, err
	}
	if options.Betas, err = popStringArrayParameter(parameters, "betas"); err != nil {
		return nil, err
	}
	delete(parameters, "max_retries")
	options.Parameters = make(map[string]json.RawMessage, len(parameters))
	for name, value := range parameters {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return nil, fmt.Errorf("%s is not JSON-compatible", name)
		}
		options.Parameters[name] = encoded
	}
	if construction.HasMaxRetries {
		options.RetryBackoff = retryBackoff(construction.MaxRetries)
	}
	return anthropic.New(credential.Credential.APIKey.Key, spec.Model, options), nil
}

func anthropicMessagesEndpoint(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" || strings.HasSuffix(value, "/messages") {
		return value
	}
	if strings.HasSuffix(value, "/v1") {
		return value + "/messages"
	}
	return value + "/v1/messages"
}

func popRawArrayParameter(values map[string]any, name string) ([]json.RawMessage, error) {
	value, exists := values[name]
	if !exists {
		return nil, nil
	}
	delete(values, name)
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	result := make([]json.RawMessage, len(items))
	for index, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("%s item %d is not JSON-compatible", name, index)
		}
		result[index] = encoded
	}
	return result, nil
}

func popStringArrayParameter(values map[string]any, name string) ([]string, error) {
	value, exists := values[name]
	if !exists {
		return nil, nil
	}
	delete(values, name)
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
	result := make([]string, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("%s item %d must be a non-empty string", name, index)
		}
		result[index] = text
	}
	return result, nil
}

func claudeAgentModelFactory(_ context.Context, spec modelconfig.Spec, _ dacredential.Resolution, construction modelconfig.Construction) (damodel.Chat, error) {
	parameters := cloneModelParameters(construction.Parameters)
	cliPath, err := popStringParameter(parameters, "cli_path")
	if err != nil {
		return nil, err
	}
	options := claudeagent.Options{CLIPath: cliPath}
	for name, destination := range map[string]*int{
		"context_window": &options.ContextWindow, "max_output_tokens": &options.MaxOutputTokens,
	} {
		value, exists := parameters[name]
		if !exists {
			continue
		}
		parsed, ok := modelInteger(value)
		if !ok || parsed < 1 {
			return nil, fmt.Errorf("%s must be a positive integer", name)
		}
		*destination = parsed
		delete(parameters, name)
	}
	if len(parameters) != 0 {
		names := make([]string, 0, len(parameters))
		for name := range parameters {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("Claude agent adapter does not support model parameters: %s", strings.Join(names, ", "))
	}
	return claudeagent.New(spec.Model, options), nil
}

func resolveOAuthAuthentication(ctx context.Context, stateDirectory string, stderr io.Writer, hooks authenticationHooks) (modelAuthentication, error) {
	return resolveOAuthAuthenticationWithOptions(ctx, stateDirectory, stderr, hooks, authenticationResolveOptions{InteractiveLogin: true})
}

func resolveOAuthAuthenticationWithOptions(ctx context.Context, stateDirectory string, stderr io.Writer, hooks authenticationHooks, options authenticationResolveOptions) (modelAuthentication, error) {
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
