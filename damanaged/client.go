// Package damanaged provides a bounded client for the managed-agent API.
package damanaged

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const agentsPath = "/v1/deepagents/agents"

const hubPath = "/v1/platform/hub"

// Agent is one provider-owned managed-agent resource.
type Agent map[string]any

// APIError is a bounded remote error response.
type APIError struct {
	Status int
	Code   string
	Detail string
	Type   string
}

func (err *APIError) Error() string {
	if err == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("HTTP %d", err.Status)}
	if err.Code != "" {
		parts = append(parts, err.Code)
	}
	if err.Detail != "" {
		parts = append(parts, err.Detail)
	}
	return strings.Join(parts, " — ")
}

// Options controls finite client work. Its zero value uses 50-item pages, at
// most 100 pages, 2 MiB responses, 30-second requests, and one delayed retry
// for read-only requests or creates carrying a stable idempotency key.
type Options struct {
	PageSize         int
	MaxPages         int
	MaxResponseBytes int64
	MaxRequestBytes  int64
	RequestTimeout   time.Duration
	RetryDelay       time.Duration
}

// Client is an authenticated managed-agent API client.
type Client struct {
	httpClient *http.Client
	endpoint   *url.URL
	apiKey     string
	options    Options
}

// Endpoint returns the normalized credential-free API origin.
func (client *Client) Endpoint() string {
	client.requireInitialized()
	return client.endpoint.String()
}

// New constructs a client from required transport, HTTPS endpoint, and API
// key without performing network work. Invalid static configuration panics.
func New(httpClient *http.Client, endpoint, apiKey string, options Options) *Client {
	if httpClient == nil {
		panic("managed-agent HTTP client is required")
	}
	endpointURL, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || endpointURL.Scheme != "https" || endpointURL.Host == "" || endpointURL.User != nil || endpointURL.RawQuery != "" || endpointURL.Fragment != "" {
		panic("managed-agent endpoint must be an HTTPS origin without credentials, query, or fragment")
	}
	endpointURL.Path = strings.TrimRight(endpointURL.Path, "/")
	endpointURL.RawPath = ""
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || len(apiKey) > 16<<10 || strings.ContainsAny(apiKey, "\x00\r\n") {
		panic("managed-agent API key is invalid")
	}
	options = options.withDefaults()
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{httpClient: &clientCopy, endpoint: endpointURL, apiKey: apiKey, options: options}
}

func (options Options) withDefaults() Options {
	if options.PageSize < 0 || options.MaxPages < 0 || options.MaxResponseBytes < 0 || options.MaxRequestBytes < 0 || options.RequestTimeout < 0 || options.RetryDelay < 0 {
		panic("managed-agent client limits cannot be negative")
	}
	if options.PageSize == 0 {
		options.PageSize = 50
	}
	if options.MaxPages == 0 {
		options.MaxPages = 100
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = 2 << 20
	}
	if options.MaxRequestBytes == 0 {
		options.MaxRequestBytes = 10 << 20
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 30 * time.Second
	}
	if options.RetryDelay == 0 {
		options.RetryDelay = time.Second
	}
	if options.PageSize > 200 || options.MaxPages > 1000 || options.MaxResponseBytes > 16<<20 || options.MaxRequestBytes > 16<<20 || options.RequestTimeout > 5*time.Minute || options.RetryDelay > 30*time.Second {
		panic("managed-agent client limits exceed hard maxima")
	}
	return options
}

// ListAgents returns every bounded page, optionally filtered by exact name.
func (client *Client) ListAgents(ctx context.Context, name string) ([]Agent, error) {
	client.requireInitialized()
	name = strings.TrimSpace(name)
	if len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
		return nil, errors.New("managed-agent name filter is invalid")
	}
	var agents []Agent
	cursor := ""
	seen := map[string]struct{}{}
	for page := 0; page < client.options.MaxPages; page++ {
		query := url.Values{"page_size": {strconv.Itoa(client.options.PageSize)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		if name != "" {
			query.Set("name", name)
		}
		body, err := client.request(ctx, http.MethodGet, agentsPath, query, nil)
		if err != nil {
			return nil, err
		}
		var response struct {
			Items      []Agent `json:"items"`
			NextCursor string  `json:"next_cursor"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, fmt.Errorf("decode managed-agent list: %w", err)
		}
		if len(response.Items) > client.options.PageSize {
			return nil, errors.New("managed-agent page exceeds requested size")
		}
		agents = append(agents, response.Items...)
		cursor = response.NextCursor
		if cursor == "" {
			return agents, nil
		}
		if len(cursor) > 4096 {
			return nil, errors.New("managed-agent cursor is too large")
		}
		if _, exists := seen[cursor]; exists {
			return nil, errors.New("managed-agent pagination cursor repeated")
		}
		seen[cursor] = struct{}{}
	}
	return nil, errors.New("managed-agent listing exceeded page limit")
}

// GetAgent fetches one exact agent ID. includeFiles requests its directory
// projection from the provider.
func (client *Client) GetAgent(ctx context.Context, agentID string, includeFiles bool) (Agent, error) {
	client.requireInitialized()
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	query := url.Values{}
	if includeFiles {
		query.Set("include_files", "true")
	}
	body, err := client.request(ctx, http.MethodGet, agentsPath+"/"+url.PathEscape(agentID), query, nil)
	if err != nil {
		return nil, err
	}
	var agent Agent
	if err := json.Unmarshal(body, &agent); err != nil {
		return nil, fmt.Errorf("decode managed agent: %w", err)
	}
	if agent == nil {
		return nil, errors.New("managed-agent response is not an object")
	}
	return agent, nil
}

// DeleteAgent permanently deletes one exact agent ID.
func (client *Client) DeleteAgent(ctx context.Context, agentID string) error {
	client.requireInitialized()
	if err := validateAgentID(agentID); err != nil {
		return err
	}
	_, err := client.request(ctx, http.MethodDelete, agentsPath+"/"+url.PathEscape(agentID), nil, nil)
	return err
}

// GetAgentHealth fetches the provider-owned health projection.
func (client *Client) GetAgentHealth(ctx context.Context, agentID string) (any, error) {
	client.requireInitialized()
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	body, err := client.request(ctx, http.MethodGet, agentsPath+"/"+url.PathEscape(agentID)+"/health", nil, nil)
	if err != nil {
		return nil, err
	}
	var health any
	if err := json.Unmarshal(body, &health); err != nil {
		return nil, fmt.Errorf("decode managed-agent health: %w", err)
	}
	return health, nil
}

// CreateAgent creates a managed agent from a validated project payload. The
// required stable idempotency key identifies one logical project creation.
func (client *Client) CreateAgent(ctx context.Context, payload map[string]any, idempotencyKey string) (Agent, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}
	return client.writeAgent(ctx, http.MethodPost, agentsPath, payload, idempotencyKey)
}

// PatchAgent updates metadata for one exact managed agent.
func (client *Client) PatchAgent(ctx context.Context, agentID string, payload map[string]any) (Agent, error) {
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	return client.writeAgent(ctx, http.MethodPatch, agentsPath+"/"+url.PathEscape(agentID), payload, "")
}

func (client *Client) writeAgent(ctx context.Context, method, path string, payload map[string]any, idempotencyKey string) (Agent, error) {
	client.requireInitialized()
	body, err := client.marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	response, err := client.requestWithIdempotency(ctx, method, path, nil, body, idempotencyKey)
	if err != nil {
		return nil, err
	}
	var agent Agent
	if err := json.Unmarshal(response, &agent); err != nil || agent == nil {
		return nil, errors.New("managed-agent response is not an object")
	}
	return agent, nil
}

// DirectoryFile is one file value in a managed-directory commit. A nil map
// value represents deletion.
type DirectoryFile struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// GetAgentDirectory fetches the provider-owned directory projection.
func (client *Client) GetAgentDirectory(ctx context.Context, agentID string) (map[string]any, error) {
	client.requireInitialized()
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	body, err := client.request(ctx, http.MethodGet, hubPath+"/repos/-/"+url.PathEscape(agentID)+"/directories", nil, nil)
	if err != nil {
		return nil, err
	}
	var directory map[string]any
	if err := json.Unmarshal(body, &directory); err != nil || directory == nil {
		return nil, errors.New("managed-agent directory response is not an object")
	}
	return directory, nil
}

// CommitAgentDirectory atomically commits a bounded file delta.
func (client *Client) CommitAgentDirectory(ctx context.Context, agentID string, files map[string]*DirectoryFile, parentCommit string) (map[string]any, error) {
	client.requireInitialized()
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	if len(files) == 0 || len(files) > maxProjectEntries {
		return nil, errors.New("managed-agent directory delta is empty or too large")
	}
	for filePath, entry := range files {
		if !validManagedPath(filePath) {
			return nil, fmt.Errorf("managed-agent directory path %q is invalid", filePath)
		}
		if entry != nil && (entry.Type != "file" || len(entry.Content) > maxProjectFileBytes || !utf8.ValidString(entry.Content)) {
			return nil, fmt.Errorf("managed-agent directory file %q is invalid", filePath)
		}
	}
	if len(parentCommit) > 512 || strings.ContainsAny(parentCommit, "\x00\r\n") {
		return nil, errors.New("managed-agent parent commit is invalid")
	}
	payload := map[string]any{"files": files}
	if parentCommit != "" {
		payload["parent_commit"] = parentCommit
	}
	encoded, err := client.marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	body, err := client.request(ctx, http.MethodPost, hubPath+"/repos/-/"+url.PathEscape(agentID)+"/directories/commits", nil, encoded)
	if err != nil {
		return nil, err
	}
	var commit map[string]any
	if err := json.Unmarshal(body, &commit); err != nil || commit == nil {
		return nil, errors.New("managed-agent commit response is not an object")
	}
	return commit, nil
}

func (client *Client) marshalPayload(payload map[string]any) ([]byte, error) {
	if payload == nil {
		return nil, errors.New("managed-agent payload is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode managed-agent payload: %w", err)
	}
	if int64(len(body)) > client.options.MaxRequestBytes {
		return nil, errors.New("managed-agent request exceeds size limit")
	}
	return body, nil
}

func (client *Client) request(ctx context.Context, method, path string, query url.Values, payload []byte) ([]byte, error) {
	return client.requestWithIdempotency(ctx, method, path, query, payload, "")
}

func (client *Client) requestWithIdempotency(ctx context.Context, method, path string, query url.Values, payload []byte, idempotencyKey string) ([]byte, error) {
	requestURL := *client.endpoint
	requestURL.Path = strings.TrimRight(client.endpoint.Path, "/") + path
	requestURL.RawQuery = query.Encode()
	var lastStatus int
	var lastBody []byte
	attempts := 1
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions || idempotencyKey != "" {
		attempts = 2
	}
	for attempt := 0; attempt < attempts; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, client.options.RequestTimeout)
		request, err := http.NewRequestWithContext(requestCtx, method, requestURL.String(), bytes.NewReader(payload))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("create managed-agent request: %w", err)
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("X-Api-Key", client.apiKey)
		if idempotencyKey != "" {
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.httpClient.Do(request)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt+1 >= attempts {
				return nil, fmt.Errorf("managed-agent request failed: %w", err)
			}
			timer := time.NewTimer(client.options.RetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, client.options.MaxResponseBytes+1))
		closeErr := response.Body.Close()
		cancel()
		if readErr != nil {
			return nil, fmt.Errorf("read managed-agent response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close managed-agent response: %w", closeErr)
		}
		if int64(len(body)) > client.options.MaxResponseBytes {
			return nil, errors.New("managed-agent response exceeds size limit")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return body, nil
		}
		if response.StatusCode >= 400 && response.StatusCode < 500 {
			return nil, decodeAPIError(response.StatusCode, body)
		}
		lastStatus, lastBody = response.StatusCode, body
		if attempt+1 < attempts {
			timer := time.NewTimer(client.options.RetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, decodeAPIError(lastStatus, lastBody)
}

func validateIdempotencyKey(value string) error {
	if len(value) < 16 || len(value) > 128 || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("managed-agent idempotency key is invalid")
	}
	return nil
}

func validManagedPath(filePath string) bool {
	if filePath == "AGENTS.md" || filePath == "tools.json" {
		return true
	}
	if len(filePath) == 0 || len(filePath) > 4096 || !fs.ValidPath(filePath) || strings.Contains(filePath, `\`) {
		return false
	}
	return strings.HasPrefix(filePath, "skills/") || strings.HasPrefix(filePath, "subagents/")
}

func decodeAPIError(status int, body []byte) error {
	var envelope struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Type   string `json:"type"`
	}
	_ = json.Unmarshal(body, &envelope)
	if envelope.Detail == "" {
		envelope.Detail = strings.TrimSpace(string(body))
	}
	return &APIError{
		Status: status,
		Code:   boundedText(envelope.Code, 256),
		Detail: boundedText(envelope.Detail, 4096),
		Type:   boundedText(envelope.Type, 256),
	}
}

func validateAgentID(agentID string) error {
	if agentID == "" || agentID != strings.TrimSpace(agentID) || len(agentID) > 256 || strings.ContainsAny(agentID, "/\\?#\x00\r\n") {
		return errors.New("managed-agent ID is invalid")
	}
	return nil
}

func boundedText(value string, limit int) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " ")
	if len(value) > limit {
		return value[:limit] + "…"
	}
	return value
}

func (client *Client) requireInitialized() {
	if client == nil || client.httpClient == nil || client.endpoint == nil || client.apiKey == "" {
		panic("initialized managed-agent client is required")
	}
}
