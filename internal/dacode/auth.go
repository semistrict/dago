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

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
)

const (
	oauthStoreFilename = "openai-oauth.json"
	oauthLoginTimeout  = 5 * time.Minute
)

type modelAuthentication struct {
	apiKey       string
	credentials  openai.CredentialSource
	subscription bool
}

type authenticationHooks struct {
	load    func(string, openai.OAuthOptions) (*openai.OAuthSession, error)
	login   func(context.Context, openai.OAuthOptions) (*openai.OAuthSession, error)
	openURL func(string) error
}

func defaultAuthenticationHooks() authenticationHooks {
	return authenticationHooks{
		load: openai.LoadOAuthSession, login: openai.Login, openURL: openExternalURL,
	}
}

func resolveAuthentication(ctx context.Context, apiKey, stateDirectory string, stderr io.Writer, hooks authenticationHooks) (modelAuthentication, error) {
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

	fmt.Fprintln(stderr, "No API key found. Starting OpenAI subscription sign-in…")
	loginCtx, cancel := context.WithTimeout(ctx, oauthLoginTimeout)
	defer cancel()
	session, err = hooks.login(loginCtx, openai.OAuthOptions{
		StorePath: storePath,
		OpenURL: func(value string) error {
			fmt.Fprintf(stderr, "Open this URL to sign in:\n%s\n", value)
			if hooks.openURL != nil {
				if openErr := hooks.openURL(value); openErr != nil {
					fmt.Fprintf(stderr, "Could not open a browser automatically: %v\n", openErr)
				}
			}
			return nil
		},
	})
	if err != nil {
		return modelAuthentication{}, fmt.Errorf("OpenAI subscription sign-in: %w", err)
	}
	fmt.Fprintln(stderr, "Sign-in complete.")
	return modelAuthentication{credentials: session, subscription: true}, nil
}

func (authentication modelAuthentication) newModel(model, baseURL string) (damodel.Chat, error) {
	options := openai.Options{Model: model, ContextWindow: 128_000}
	if authentication.subscription {
		options.ContextWindow = 272_000
		return openai.NewSubscription(authentication.credentials, options)
	}
	options.BaseURL = baseURL
	return openai.NewAPIKey(authentication.apiKey, options)
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
