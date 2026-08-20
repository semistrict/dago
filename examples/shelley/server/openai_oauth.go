package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/semistrict/dago/damodel"
	dopenai "github.com/semistrict/dago/daproviders/openai"

	"github.com/semistrict/dago/examples/shelley/models"
)

const (
	OpenAISubscriptionModelID = "gpt-5.6-luna"
	openAIOAuthTimeout        = 5 * time.Minute
	openAIOAuthURLTimeout     = 10 * time.Second
)

// OpenAIOAuthStatus is the non-secret state exposed to the web UI.
type OpenAIOAuthStatus struct {
	State   string `json:"state"`
	Ready   bool   `json:"ready"`
	ModelID string `json:"model_id"`
	Error   string `json:"error,omitempty"`
}

type openAIOAuthLogin func(context.Context, func(string) error, dopenai.OAuthOptions) (dopenai.CredentialSource, error)

// OpenAIOAuth owns Shelley's caller-specific subscription session. The
// provider owns token refresh and atomic 0600 persistence; this controller
// owns login state and model-catalog synchronization.
type OpenAIOAuth struct {
	mu        sync.RWMutex
	storePath string
	session   dopenai.CredentialSource
	state     string
	errorText string
	cancel    context.CancelFunc
	attempt   uint64
	onChange  func(context.Context) error
	login     openAIOAuthLogin
	logger    *slog.Logger
}

// NewOpenAIOAuth loads a previously persisted session when present. A missing
// file is the normal signed-out state; malformed credential files remain
// visible as a failed state instead of being silently discarded.
func NewOpenAIOAuth(storePath string, logger *slog.Logger) *OpenAIOAuth {
	if logger == nil {
		logger = slog.Default()
	}
	controller := &OpenAIOAuth{
		storePath: storePath,
		login: func(ctx context.Context, openURL func(string) error, options dopenai.OAuthOptions) (dopenai.CredentialSource, error) {
			return dopenai.Login(ctx, openURL, options)
		},
		logger: logger,
	}
	if storePath == "" {
		controller.state = "disabled"
		controller.errorText = "OpenAI subscription sign-in is not configured"
		return controller
	}
	session, err := dopenai.LoadOAuthSession(storePath, dopenai.OAuthOptions{})
	if err == nil {
		controller.session = session
		controller.state = "complete"
		return controller
	}
	if !errors.Is(err, os.ErrNotExist) {
		controller.state = "failed"
		controller.errorText = err.Error()
	}
	return controller
}

// SetOnChange installs the catalog refresh invoked after sign-in and sign-out.
func (controller *OpenAIOAuth) SetOnChange(callback func(context.Context) error) {
	controller.mu.Lock()
	controller.onChange = callback
	controller.mu.Unlock()
}

func (controller *OpenAIOAuth) Status() OpenAIOAuthStatus {
	if controller == nil {
		return OpenAIOAuthStatus{State: "disabled", ModelID: OpenAISubscriptionModelID}
	}
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	return OpenAIOAuthStatus{
		State: controller.state, Ready: controller.session != nil && controller.state == "complete",
		ModelID: OpenAISubscriptionModelID, Error: controller.errorText,
	}
}

// BuiltModels materializes the authenticated subscription model through
// dago's native OpenAI provider. It intentionally returns no model while
// signed out or while a new login is pending.
func (controller *OpenAIOAuth) BuiltModels() ([]models.Built, error) {
	if controller == nil {
		return nil, nil
	}
	controller.mu.RLock()
	session := controller.session
	controller.mu.RUnlock()
	if session == nil {
		return nil, nil
	}
	chat := dopenai.NewSubscription(session, OpenAISubscriptionModelID, dopenai.Options{
		ContextWindow: 272000, MaxOutputTokens: 32768,
		DefaultReasoning: &damodel.Reasoning{Effort: "medium", Summary: "auto"}, WebSearch: true,
	})
	profiledChat := damodel.WithProfile(chat, func(profile *damodel.Profile) {
		profile.SupportsImages = true
		profile.SupportsReasoning = true
		profile.SupportsWebSearch = true
		profile.MaxImageBytes = 20 * 1024 * 1024
		profile.ReasoningLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}
		profile.DefaultReasoningLevel = "medium"
	})
	catalog := models.ByID(OpenAISubscriptionModelID)
	tags := ""
	if catalog != nil {
		tags = catalog.Tags
	}
	return []models.Built{{
		ID: OpenAISubscriptionModelID, DisplayName: "GPT-5.6 Luna",
		Provider: models.ProviderOpenAI, Source: "OpenAI subscription", Tags: tags,
		Chat: profiledChat, APIType: models.APITypeOpenAIResponses, BaseURL: "https://chatgpt.com",
	}}, nil
}

// Start begins the local-loopback PKCE flow and returns as soon as the
// authorization URL is ready. Completion continues in the background.
func (controller *OpenAIOAuth) Start() (string, error) {
	if controller == nil || controller.storePath == "" {
		return "", fmt.Errorf("OpenAI subscription sign-in is not configured")
	}
	controller.mu.Lock()
	if controller.state == "pending" {
		controller.mu.Unlock()
		return "", fmt.Errorf("OpenAI subscription sign-in is already pending")
	}
	ctx, cancel := context.WithTimeout(context.Background(), openAIOAuthTimeout)
	controller.attempt++
	attempt := controller.attempt
	controller.cancel = cancel
	controller.state = "pending"
	controller.errorText = ""
	login := controller.login
	storePath := controller.storePath
	controller.mu.Unlock()

	urls := make(chan string, 1)
	results := make(chan struct {
		session dopenai.CredentialSource
		err     error
	}, 1)
	go func() {
		session, err := login(ctx, func(value string) error {
			select {
			case urls <- value:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, dopenai.OAuthOptions{StorePath: storePath})
		results <- struct {
			session dopenai.CredentialSource
			err     error
		}{session: session, err: err}
	}()

	select {
	case authorizationURL := <-urls:
		go controller.finish(ctx, cancel, attempt, results)
		return authorizationURL, nil
	case result := <-results:
		cancel()
		controller.setFailure(attempt, result.err, "authorization stopped before producing a URL")
		return "", fmt.Errorf("%s", controller.Status().Error)
	case <-time.After(openAIOAuthURLTimeout):
		cancel()
		controller.setFailure(attempt, nil, "authorization URL was not produced")
		return "", fmt.Errorf("authorization URL was not produced")
	}
}

func (controller *OpenAIOAuth) finish(ctx context.Context, cancel context.CancelFunc, attempt uint64, results <-chan struct {
	session dopenai.CredentialSource
	err     error
}) {
	defer cancel()
	result := <-results
	if result.err != nil {
		controller.setFailure(attempt, result.err, "subscription sign-in failed")
		return
	}
	controller.mu.Lock()
	if controller.attempt != attempt {
		controller.mu.Unlock()
		return
	}
	controller.session = result.session
	callback := controller.onChange
	controller.mu.Unlock()
	if callback != nil {
		refreshCtx, refreshCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := callback(refreshCtx)
		refreshCancel()
		if err != nil {
			controller.setFailure(attempt, err, "refresh models after sign-in")
			return
		}
	}
	controller.mu.Lock()
	if controller.attempt != attempt {
		controller.mu.Unlock()
		return
	}
	controller.state = "complete"
	controller.errorText = ""
	controller.cancel = nil
	controller.mu.Unlock()
}

func (controller *OpenAIOAuth) setFailure(attempt uint64, err error, fallback string) {
	message := fallback
	if err != nil {
		message = err.Error()
	}
	controller.mu.Lock()
	if controller.attempt != attempt {
		controller.mu.Unlock()
		return
	}
	controller.state = "failed"
	controller.errorText = message
	controller.cancel = nil
	controller.mu.Unlock()
	controller.logger.Error("OpenAI subscription sign-in failed", "error", message)
}

// Clear removes the caller-owned token file and refreshes the live catalog.
func (controller *OpenAIOAuth) Clear(ctx context.Context) error {
	if controller == nil {
		return nil
	}
	controller.mu.Lock()
	controller.attempt++
	if controller.cancel != nil {
		controller.cancel()
	}
	controller.cancel = nil
	controller.session = nil
	controller.state = ""
	controller.errorText = ""
	callback := controller.onChange
	storePath := controller.storePath
	controller.mu.Unlock()
	var removeErr error
	if storePath != "" {
		removeErr = os.Remove(storePath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
	}
	var refreshErr error
	if callback != nil {
		refreshErr = callback(ctx)
	}
	return errors.Join(removeErr, refreshErr)
}

func (controller *OpenAIOAuth) Close() {
	if controller == nil {
		return
	}
	controller.mu.Lock()
	if controller.cancel != nil {
		controller.cancel()
	}
	controller.cancel = nil
	controller.mu.Unlock()
}

func (s *Server) SetOpenAIOAuth(controller *OpenAIOAuth) {
	s.openAIOAuth = controller
	if controller == nil {
		return
	}
	controller.SetOnChange(func(ctx context.Context) error {
		_, err := s.refreshModels(ctx)
		return err
	})
}

func (s *Server) handleOpenAIOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.openAIOAuth.Status())
}

func (s *Server) handleOpenAIOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.openAIOAuth == nil {
		http.Error(w, "OpenAI subscription sign-in is not configured", http.StatusNotImplemented)
		return
	}
	authorizationURL, err := s.openAIOAuth.Start()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"authorization_url": authorizationURL})
}

func (s *Server) handleOpenAIOAuthClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.openAIOAuth == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.openAIOAuth.Clear(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
