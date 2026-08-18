package dacode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/semistrict/dago/datalon/tracing"
)

const (
	defaultLangSmithProjectLookupTimeout = 2 * time.Second
	defaultLangSmithProjectResponseBytes = 64 << 10
	maxLangSmithProjectResponseBytes     = 1 << 20
	maxLangSmithProjectNameBytes         = 256
	maxLangSmithProjectCredentialBytes   = 64 << 10
)

type langSmithProjectLookupOptions struct {
	Timeout          time.Duration
	MaxResponseBytes int64
}

func (options langSmithProjectLookupOptions) withDefaults() langSmithProjectLookupOptions {
	if options.Timeout < 0 || options.MaxResponseBytes < 0 {
		panic("dacode: LangSmith project lookup limits cannot be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = defaultLangSmithProjectLookupTimeout
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = defaultLangSmithProjectResponseBytes
	}
	if options.Timeout > time.Minute || options.MaxResponseBytes > maxLangSmithProjectResponseBytes {
		panic("dacode: LangSmith project lookup limits exceed hard maxima")
	}
	return options
}

// langSmithProjectLookup resolves project IDs through the LangSmith API and
// constructs UI links from a separately trusted web origin. Secret-bearing
// provider errors and response bodies never cross this boundary.
type langSmithProjectLookup struct {
	client      http.Client
	apiEndpoint string
	webEndpoint string
	apiKey      string
	options     langSmithProjectLookupOptions
}

// newLangSmithProjectLookup performs no network I/O. Required network,
// origins, and credential dependencies are positional; optional finite limits
// have useful zero defaults.
func newLangSmithProjectLookup(client *http.Client, apiEndpoint, webEndpoint, apiKey string, options langSmithProjectLookupOptions) *langSmithProjectLookup {
	if client == nil {
		panic("dacode: LangSmith project HTTP client is required")
	}
	apiEndpoint = normalizedTraceOrigin(apiEndpoint, true)
	webEndpoint = normalizedTraceOrigin(webEndpoint, false)
	apiKey = strings.TrimSpace(apiKey)
	if apiEndpoint == "" || webEndpoint == "" || apiKey == "" || len(apiKey) > maxLangSmithProjectCredentialBytes || strings.IndexFunc(apiKey, unicode.IsControl) >= 0 {
		panic("dacode: complete LangSmith project lookup configuration is required")
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &langSmithProjectLookup{client: copyClient, apiEndpoint: apiEndpoint, webEndpoint: webEndpoint, apiKey: apiKey, options: options.withDefaults()}
}

func (lookup *langSmithProjectLookup) String() string {
	return "langSmithProjectLookup(<redacted>)"
}

func (lookup *langSmithProjectLookup) GoString() string { return lookup.String() }

func (lookup *langSmithProjectLookup) ProjectURL(ctx context.Context, project string) (string, error) {
	if ctx == nil {
		panic("dacode: LangSmith project lookup context is required")
	}
	if lookup == nil || lookup.apiEndpoint == "" || lookup.webEndpoint == "" || lookup.apiKey == "" {
		panic("dacode: initialized LangSmith project lookup is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validLangSmithProjectName(project) {
		return "", tracing.ErrProjectLookup
	}
	lookupContext := ctx
	cancel := func() {}
	if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > lookup.options.Timeout {
		lookupContext, cancel = context.WithTimeout(ctx, lookup.options.Timeout)
	}
	defer cancel()
	endpoint, err := url.Parse(lookup.apiEndpoint + "/api/v1/sessions")
	if err != nil {
		return "", tracing.ErrProjectLookup
	}
	query := endpoint.Query()
	query.Set("name", project)
	query.Set("limit", "2")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(lookupContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", tracing.ErrProjectLookup
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-api-key", lookup.apiKey)
	response, err := lookup.client.Do(request)
	if err != nil {
		if lookupContext.Err() != nil {
			return "", lookupContext.Err()
		}
		return "", tracing.ErrProjectLookup
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", tracing.ErrProjectLookup
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, lookup.options.MaxResponseBytes+1))
	if err != nil || len(payload) == 0 || int64(len(payload)) > lookup.options.MaxResponseBytes {
		return "", tracing.ErrProjectLookup
	}
	var projects []struct {
		ID       string `json:"id"`
		TenantID string `json:"tenant_id"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(payload, &projects) != nil || len(projects) == 0 || len(projects) > 2 {
		return "", tracing.ErrProjectLookup
	}
	var matched *struct {
		ID       string `json:"id"`
		TenantID string `json:"tenant_id"`
		Name     string `json:"name"`
	}
	for index := range projects {
		candidate := &projects[index]
		if candidate.Name != project {
			continue
		}
		if matched != nil {
			return "", tracing.ErrProjectLookup
		}
		matched = candidate
	}
	if matched == nil || !validLangSmithURLIdentifier(matched.TenantID) || !validLangSmithURLIdentifier(matched.ID) {
		return "", tracing.ErrProjectLookup
	}
	return lookup.webEndpoint + "/o/" + url.PathEscape(matched.TenantID) + "/projects/p/" + url.PathEscape(matched.ID), nil
}

func normalizedTraceOrigin(raw string, allowPath bool) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n") {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if !allowPath && strings.Trim(parsed.EscapedPath(), "/") != "" {
		return ""
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String()
}

func validLangSmithProjectName(value string) bool {
	return value != "" && len(value) <= maxLangSmithProjectNameBytes && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validLangSmithURLIdentifier(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

var _ tracing.ProjectLookup = (*langSmithProjectLookup)(nil)
