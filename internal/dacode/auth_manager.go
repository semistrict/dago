package dacode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	maxAuthManagerRows       = 128
	maxAuthManagerSecretSize = 64 << 10
	maxAuthManagerURLSize    = 4096
	maxAuthManagerNoticeSize = 512
)

type authManagerMode uint8

const (
	authManagerList authManagerMode = iota
	authManagerAPIKey
	authManagerSubscription
	authManagerRemoval
)

type authManagerActionKind uint8

const (
	authManagerNoAction authManagerActionKind = iota
	authManagerSaveAPIKey
	authManagerStartSubscription
	authManagerRemoveCredential
)

type authManagerAction struct {
	kind       authManagerActionKind
	provider   string
	secret     []byte
	generation uint64
}

func (action authManagerAction) String() string {
	return fmt.Sprintf("authManagerAction(kind=%d,provider=%s,<redacted>)", action.kind, action.provider)
}

func (action authManagerAction) GoString() string { return action.String() }

// consumeSecret transfers an API key to the integration and clears the
// action-owned copy. Other action kinds return an empty string.
func (action *authManagerAction) consumeSecret() string {
	if action == nil || action.kind != authManagerSaveAPIKey {
		return ""
	}
	secret := string(action.secret)
	clear(action.secret)
	action.secret = nil
	return secret
}

type authSubscriptionPhase uint8

const (
	authSubscriptionIdle authSubscriptionPhase = iota
	authSubscriptionPreparing
	authSubscriptionAuthorize
	authSubscriptionWaiting
	authSubscriptionSucceeded
	authSubscriptionCancelled
	authSubscriptionFailed
)

type authManagerRow struct {
	provider    string
	label       string
	status      string
	source      string
	detail      string
	service     bool
	oauthOnly   bool
	configured  bool
	removable   bool
	environment string
}

type authSecretInput struct {
	value []byte
}

func (input authSecretInput) String() string   { return "authSecretInput(<redacted>)" }
func (input authSecretInput) GoString() string { return input.String() }

func (input *authSecretInput) append(text string) error {
	if input == nil {
		panic("dacode: API-key input is required")
	}
	if !utf8.ValidString(text) {
		return errors.New("API key contains invalid text")
	}
	for _, character := range text {
		if unicode.IsControl(character) {
			return errors.New("API key cannot contain control characters")
		}
	}
	if len(input.value)+len(text) > maxAuthManagerSecretSize {
		return errors.New("API key exceeds the input limit")
	}
	input.value = append(input.value, text...)
	return nil
}

func (input *authSecretInput) backspace() {
	if input == nil || len(input.value) == 0 {
		return
	}
	_, size := utf8.DecodeLastRune(input.value)
	clear(input.value[len(input.value)-size:])
	input.value = input.value[:len(input.value)-size]
}

func (input *authSecretInput) reset() {
	if input == nil {
		return
	}
	clear(input.value)
	input.value = nil
}

func (input *authSecretInput) take() ([]byte, bool) {
	if input == nil {
		return nil, false
	}
	trimmed := bytes.TrimSpace(input.value)
	if len(trimmed) == 0 {
		return nil, false
	}
	secret := append([]byte(nil), trimmed...)
	input.reset()
	return secret, true
}

type authManagerState struct {
	rows                   []authManagerRow
	selected               int
	mode                   authManagerMode
	target                 string
	secret                 authSecretInput
	subscriptionPhase      authSubscriptionPhase
	subscriptionGeneration uint64
	authorizeURL           string
	notice                 string
}

func (state authManagerState) String() string {
	return fmt.Sprintf("authManagerState(rows=%d,selected=%d,mode=%d,target=%s,<redacted>)", len(state.rows), state.selected, state.mode, state.target)
}

func (state authManagerState) GoString() string { return state.String() }

// authManager binds credential discovery dependencies to a pure interaction
// state. Construction performs no I/O; refresh is separately cancellable.
type authManager struct {
	store  *dacredential.Store
	lookup dacredential.EnvironmentLookup
	state  authManagerState
}

func newAuthManager(store *dacredential.Store, lookup dacredential.EnvironmentLookup) *authManager {
	if store == nil {
		panic("dacode: credential store is required")
	}
	if lookup == nil {
		panic("dacode: environment lookup is required")
	}
	return &authManager{store: store, lookup: lookup}
}

// refresh resolves a deterministic provider/service snapshot. Subscription
// storage is owned by its provider integration, so its current presence is
// supplied explicitly rather than discovered through hidden filesystem I/O.
func (manager *authManager) refresh(ctx context.Context, subscriptionConfigured bool) error {
	if manager == nil || manager.store == nil || manager.lookup == nil {
		panic("dacode: initialized auth manager is required")
	}
	if ctx == nil {
		panic("dacode: nil auth manager context")
	}
	snapshot, err := manager.store.Load(ctx)
	if err != nil {
		manager.failRefresh()
		return err
	}
	providers, truncated := mergeAuthManagerProviders(dacredential.Providers(), snapshot.Providers())
	rows := make([]authManagerRow, 0, len(providers))
	for _, provider := range providers {
		if err := ctx.Err(); err != nil {
			return err
		}
		resolution, resolveErr := manager.store.Resolve(ctx, provider.Name, manager.lookup)
		if resolveErr != nil {
			manager.failRefresh()
			return resolveErr
		}
		rows = append(rows, makeAuthManagerRow(provider, resolution, subscriptionConfigured))
	}
	sort.SliceStable(rows, func(left, right int) bool {
		if rows[left].configured != rows[right].configured {
			return rows[left].configured
		}
		return rows[left].provider < rows[right].provider
	})
	selectedProvider := manager.state.selectedProvider()
	manager.state.rows = rows
	manager.state.selected = 0
	for index := range rows {
		if rows[index].provider == selectedProvider {
			manager.state.selected = index
			break
		}
	}
	if truncated {
		manager.state.setNotice("Some credential entries are hidden by the display limit.")
	} else if snapshot.Malformed() > 0 {
		manager.state.setNotice("Some malformed credential entries were ignored.")
	} else {
		manager.state.notice = ""
	}
	return nil
}

func (manager *authManager) failRefresh() {
	manager.state.cancel()
	manager.state.rows = nil
	manager.state.selected = 0
	manager.state.setNotice("Credential status is unavailable.")
}

func mergeAuthManagerProviders(pinned []dacredential.Provider, stored []string) ([]dacredential.Provider, bool) {
	providers := append([]dacredential.Provider(nil), pinned...)
	sort.Slice(providers, func(left, right int) bool { return providers[left].Name < providers[right].Name })
	if len(providers) >= maxAuthManagerRows {
		return providers[:maxAuthManagerRows], len(providers) > maxAuthManagerRows || len(stored) > 0
	}
	known := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		known[provider.Name] = struct{}{}
	}
	custom := make([]string, 0, len(stored))
	for _, provider := range stored {
		if _, exists := known[provider]; !exists {
			custom = append(custom, provider)
			known[provider] = struct{}{}
		}
	}
	sort.Strings(custom)
	available := maxAuthManagerRows - len(providers)
	truncated := len(custom) > available
	if truncated {
		custom = custom[:available]
	}
	for _, provider := range custom {
		providers = append(providers, dacredential.Provider{Name: provider})
	}
	return providers, truncated
}

func makeAuthManagerRow(provider dacredential.Provider, resolution dacredential.Resolution, subscriptionConfigured bool) authManagerRow {
	row := authManagerRow{
		provider:    provider.Name,
		label:       humanizeAuthProvider(provider.Name),
		service:     provider.Service,
		oauthOnly:   provider.OAuthOnly,
		environment: resolution.Environment,
	}
	if provider.OAuthOnly {
		if subscriptionConfigured {
			row.status, row.source, row.detail = "connected", "subscription", "Signed in with an OpenAI subscription"
			row.configured, row.removable = true, true
		} else {
			row.status, row.source, row.detail = "not configured", "missing", "Subscription sign-in available"
		}
		return row
	}
	switch resolution.Source {
	case dacredential.StoredSource:
		row.status, row.source, row.detail = "configured", "stored", "Saved in the credential store"
		row.configured, row.removable = true, true
	case dacredential.EnvironmentSource:
		row.status, row.source = "configured", "environment"
		row.detail = "From " + resolution.Environment
		row.configured = true
	default:
		row.status, row.source = "not configured", "missing"
		if provider.Environment == "" {
			row.detail = "API key not set"
		} else {
			row.detail = "Set an API key or " + provider.Environment
		}
	}
	return row
}

func humanizeAuthProvider(provider string) string {
	if metadata, known := dacredential.ProviderByName(provider); known && metadata.OAuthOnly {
		return "OpenAI Subscription"
	}
	if label, known := authProviderLabels[provider]; known {
		return label
	}
	words := strings.Fields(strings.ReplaceAll(provider, "_", " "))
	for index, word := range words {
		if word == "api" {
			words[index] = "API"
			continue
		}
		runes := []rune(word)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
		}
		words[index] = string(runes)
	}
	return strings.Join(words, " ")
}

var authProviderLabels = map[string]string{
	"anthropic":       "Anthropic",
	"azure_openai":    "Azure OpenAI",
	"baseten":         "Baseten",
	"cohere":          "Cohere",
	"deepseek":        "DeepSeek",
	"fireworks":       "Fireworks",
	"google_genai":    "Google Gemini",
	"google_vertexai": "Google Vertex AI",
	"groq":            "Groq",
	"huggingface":     "Hugging Face",
	"ibm":             "IBM watsonx",
	"langsmith":       "LangSmith",
	"litellm":         "LiteLLM",
	"meta":            "Meta",
	"mistralai":       "Mistral AI",
	"nvidia":          "NVIDIA",
	"openai":          "OpenAI",
	"openrouter":      "OpenRouter",
	"perplexity":      "Perplexity",
	"tavily":          "Tavily",
	"together":        "Together AI",
	"xai":             "xAI",
}

func (state *authManagerState) selectedProvider() string {
	row, ok := state.selectedRow()
	if !ok {
		return ""
	}
	return row.provider
}

func (state *authManagerState) selectedRow() (authManagerRow, bool) {
	if state == nil || state.selected < 0 || state.selected >= len(state.rows) {
		return authManagerRow{}, false
	}
	return state.rows[state.selected], true
}

func (state *authManagerState) move(delta int) {
	if state == nil || state.mode != authManagerList || len(state.rows) == 0 || delta == 0 {
		return
	}
	state.selected = (state.selected + delta) % len(state.rows)
	if state.selected < 0 {
		state.selected += len(state.rows)
	}
}

// beginSelected opens API-key entry or requests the provider-owned
// subscription flow. It never performs storage or network I/O.
func (state *authManagerState) beginSelected() authManagerAction {
	row, ok := state.selectedRow()
	if !ok || state.mode != authManagerList {
		return authManagerAction{}
	}
	state.notice = ""
	state.target = row.provider
	if row.oauthOnly {
		state.subscriptionGeneration++
		if state.subscriptionGeneration == 0 {
			state.subscriptionGeneration = 1
		}
		state.mode = authManagerSubscription
		state.subscriptionPhase = authSubscriptionPreparing
		return authManagerAction{kind: authManagerStartSubscription, provider: row.provider, generation: state.subscriptionGeneration}
	}
	state.mode = authManagerAPIKey
	state.secret.reset()
	return authManagerAction{}
}

func (state *authManagerState) appendAPIKey(text string) error {
	if state == nil || state.mode != authManagerAPIKey {
		return errors.New("API-key entry is not active")
	}
	if err := state.secret.append(text); err != nil {
		state.setNotice(err.Error())
		return err
	}
	state.notice = ""
	return nil
}

func (state *authManagerState) backspaceAPIKey() {
	if state != nil && state.mode == authManagerAPIKey {
		state.secret.backspace()
	}
}

func (state *authManagerState) submitAPIKey() authManagerAction {
	if state == nil || state.mode != authManagerAPIKey {
		return authManagerAction{}
	}
	secret, ok := state.secret.take()
	if !ok {
		state.setNotice("Enter an API key before saving.")
		return authManagerAction{}
	}
	action := authManagerAction{kind: authManagerSaveAPIKey, provider: state.target, secret: secret}
	state.mode, state.target, state.notice = authManagerList, "", ""
	return action
}

func (state *authManagerState) beginRemoval() bool {
	row, ok := state.selectedRow()
	if !ok || state.mode != authManagerList {
		return false
	}
	if !row.removable {
		if row.source == "environment" {
			state.setNotice("Environment credentials must be changed outside this application.")
		} else {
			state.setNotice("There is no stored credential to remove.")
		}
		return false
	}
	state.mode, state.target, state.notice = authManagerRemoval, row.provider, ""
	return true
}

func (state *authManagerState) confirmRemoval() authManagerAction {
	if state == nil || state.mode != authManagerRemoval || state.target == "" {
		return authManagerAction{}
	}
	action := authManagerAction{kind: authManagerRemoveCredential, provider: state.target}
	state.mode, state.target = authManagerList, ""
	return action
}

func (state *authManagerState) cancel() {
	if state == nil {
		return
	}
	state.secret.reset()
	if state.mode == authManagerSubscription {
		state.subscriptionGeneration++
		if state.subscriptionGeneration == 0 {
			state.subscriptionGeneration = 1
		}
	}
	state.mode, state.target, state.subscriptionPhase = authManagerList, "", authSubscriptionIdle
	state.authorizeURL, state.notice = "", ""
}

func (state *authManagerState) setSubscriptionPhase(phase authSubscriptionPhase) {
	if state == nil {
		return
	}
	state.setSubscriptionPhaseFor(state.subscriptionGeneration, phase)
}

// setSubscriptionPhaseFor ignores results from cancelled or superseded login
// workers. A generation is returned by beginSelected in the start action.
func (state *authManagerState) setSubscriptionPhaseFor(generation uint64, phase authSubscriptionPhase) {
	if state == nil || state.mode != authManagerSubscription {
		return
	}
	if generation == 0 || generation != state.subscriptionGeneration {
		return
	}
	switch phase {
	case authSubscriptionPreparing, authSubscriptionAuthorize, authSubscriptionWaiting,
		authSubscriptionSucceeded, authSubscriptionCancelled, authSubscriptionFailed:
		state.subscriptionPhase = phase
	default:
		state.subscriptionPhase = authSubscriptionFailed
	}
	if phase != authSubscriptionAuthorize && phase != authSubscriptionWaiting {
		state.authorizeURL = ""
	}
}

func (state *authManagerState) setSubscriptionAuthorizeURL(raw string, opened bool) error {
	if state == nil {
		return errors.New("subscription sign-in is not active")
	}
	return state.setSubscriptionAuthorizeURLFor(state.subscriptionGeneration, raw, opened)
}

// setSubscriptionAuthorizeURLFor applies only the active login generation, so
// a late callback cannot replace the URL of a newer sign-in attempt.
func (state *authManagerState) setSubscriptionAuthorizeURLFor(generation uint64, raw string, opened bool) error {
	if state == nil || state.mode != authManagerSubscription {
		return errors.New("subscription sign-in is not active")
	}
	if generation == 0 || generation != state.subscriptionGeneration {
		return errors.New("subscription sign-in was superseded")
	}
	value, err := validateSubscriptionAuthorizeURL(raw)
	if err != nil {
		state.subscriptionPhase = authSubscriptionFailed
		state.authorizeURL = ""
		return err
	}
	state.authorizeURL = value
	if opened {
		state.subscriptionPhase = authSubscriptionWaiting
	} else {
		state.subscriptionPhase = authSubscriptionAuthorize
	}
	return nil
}

// subscriptionAuthorizeURL returns the validated full URL for the clipboard
// path. The rendered preview is width-bounded and must not be used as the
// source for headless sign-in.
func (state *authManagerState) subscriptionAuthorizeURL() (string, bool) {
	if state == nil || state.mode != authManagerSubscription || state.authorizeURL == "" {
		return "", false
	}
	return state.authorizeURL, true
}

func validateSubscriptionAuthorizeURL(raw string) (string, error) {
	if raw == "" || len(raw) > maxAuthManagerURLSize || strings.ContainsAny(raw, "\x00\r\n") {
		return "", errors.New("authorization URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "auth.openai.com") || parsed.Port() != "" || parsed.User != nil || parsed.Fragment != "" || parsed.Path != "/oauth/authorize" {
		return "", errors.New("authorization URL is invalid")
	}
	allowed := map[string]struct{}{
		"response_type": {}, "client_id": {}, "redirect_uri": {}, "scope": {},
		"code_challenge": {}, "code_challenge_method": {}, "id_token_add_organizations": {},
		"state": {}, "originator": {},
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", errors.New("authorization URL is invalid")
	}
	for key := range query {
		if _, ok := allowed[key]; !ok {
			return "", errors.New("authorization URL contains secret material")
		}
	}
	return parsed.String(), nil
}

func (state *authManagerState) setNotice(notice string) {
	if state == nil {
		return
	}
	notice = singleLineTerminalSafe(notice)
	state.notice = truncateAuthText(notice, maxAuthManagerNoticeSize)
}

func (state *authManagerState) render(width, height int) string {
	if state == nil {
		return ""
	}
	width = min(max(width, 24), 160)
	height = min(max(height, 6), 80)
	var lines []string
	switch state.mode {
	case authManagerAPIKey:
		lines = state.renderAPIKey()
	case authManagerSubscription:
		lines = state.renderSubscription()
	case authManagerRemoval:
		lines = state.renderRemoval()
	default:
		lines = state.renderList(height)
	}
	for index := range lines {
		lines[index] = truncateAuthText(singleLineTerminalSafe(lines[index]), width)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func (state *authManagerState) renderList(height int) []string {
	lines := []string{"Manage credentials", ""}
	if len(state.rows) == 0 {
		lines = append(lines, "No providers or services are available.")
	} else {
		visible := min(len(state.rows), max(height-5, 1))
		start := min(max(state.selected-visible/2, 0), max(len(state.rows)-visible, 0))
		for index := start; index < start+visible; index++ {
			row := state.rows[index]
			prefix := "  "
			if index == state.selected {
				prefix = "> "
			}
			kind := "provider"
			if row.service {
				kind = "service"
			}
			lines = append(lines, fmt.Sprintf("%s%s (%s) [%s: %s] %s", prefix, row.label, kind, row.source, row.status, row.detail))
		}
	}
	if state.notice != "" {
		lines = append(lines, "", state.notice)
	}
	lines = append(lines, "Enter manage  -  Delete remove  -  Esc close")
	return lines
}

func (state *authManagerState) renderAPIKey() []string {
	entry := "not entered"
	if len(state.secret.value) > 0 {
		entry = "<hidden>"
	}
	lines := []string{"Add API key", "", "Provider: " + humanizeAuthProvider(state.target), "API key: " + entry}
	if state.notice != "" {
		lines = append(lines, "", state.notice)
	}
	return append(lines, "Enter save  -  Esc cancel")
}

func (state *authManagerState) renderSubscription() []string {
	message := "Preparing secure sign-in..."
	switch state.subscriptionPhase {
	case authSubscriptionAuthorize:
		message = "Open this URL to continue:"
	case authSubscriptionWaiting:
		message = "Waiting for sign-in to finish..."
	case authSubscriptionSucceeded:
		message = "Subscription sign-in complete."
	case authSubscriptionCancelled:
		message = "Sign-in cancelled."
	case authSubscriptionFailed:
		message = "Sign-in failed. Try again."
	}
	lines := []string{"OpenAI subscription sign-in", "", message}
	if state.authorizeURL != "" {
		lines = append(lines, state.authorizeURL)
	}
	help := "Esc cancel"
	if state.authorizeURL != "" {
		help = "c copy full URL  -  Esc cancel"
	}
	return append(lines, help)
}

func (state *authManagerState) renderRemoval() []string {
	return []string{
		"Remove credential?",
		"",
		"Remove the stored credential for " + humanizeAuthProvider(state.target) + "?",
		"Enter confirm  -  Esc cancel",
	}
}

func singleLineTerminalSafe(value string) string {
	value = unicodesecurity.RenderTerminalSafe(value)
	return strings.ReplaceAll(value, "\n", " ")
}

func truncateAuthText(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "."
	}
	if limit < 4 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}
