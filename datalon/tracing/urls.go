package tracing

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

var (
	ErrProjectLookup = errors.New("trace project lookup failed")
	ErrProjectURL    = errors.New("trace project URL is invalid")
)

// ProjectLookup resolves one project to its provider web URL. Implementations
// own authentication and network policy and must honor the context.
type ProjectLookup interface {
	ProjectURL(context.Context, string) (string, error)
}

// URLResolverOptions controls bounded project URL lookups and caching. Its
// zero value uses a two-second timeout, five-minute TTL, and 128 entries.
type URLResolverOptions struct {
	LookupTimeout        time.Duration
	CacheTTL             time.Duration
	MaxEntries           int
	MaxURLBytes          int
	MaxConcurrentLookups int
}

func (options URLResolverOptions) withDefaults() URLResolverOptions {
	if options.LookupTimeout < 0 || options.CacheTTL < 0 || options.MaxEntries < 0 || options.MaxURLBytes < 0 || options.MaxConcurrentLookups < 0 {
		panic("trace URL resolver limits cannot be negative")
	}
	if options.LookupTimeout <= 0 {
		options.LookupTimeout = 2 * time.Second
	}
	if options.CacheTTL <= 0 {
		options.CacheTTL = 5 * time.Minute
	}
	if options.MaxEntries <= 0 {
		options.MaxEntries = 128
	}
	if options.MaxURLBytes <= 0 {
		options.MaxURLBytes = 16 << 10
	}
	if options.MaxConcurrentLookups <= 0 {
		options.MaxConcurrentLookups = 8
	}
	if options.LookupTimeout > time.Minute || options.CacheTTL > 24*time.Hour || options.MaxEntries > 4096 || options.MaxURLBytes > 1<<20 || options.MaxConcurrentLookups > 64 {
		panic("trace URL resolver limits exceed hard maxima")
	}
	return options
}

type cachedProjectURL struct {
	url       string
	expiresAt time.Time
}

// URLResolver resolves and caches provider web links without opening them.
type URLResolver struct {
	lookup  ProjectLookup
	options URLResolverOptions

	mu    sync.Mutex
	cache map[string]cachedProjectURL
	gate  map[string]chan struct{}
	now   func() time.Time
	slots chan struct{}
}

// NewURLResolver constructs a resolver without network I/O.
func NewURLResolver(lookup ProjectLookup, options URLResolverOptions) *URLResolver {
	if nilValue(lookup) {
		panic("trace project lookup is required")
	}
	options = options.withDefaults()
	return &URLResolver{lookup: lookup, options: options, cache: map[string]cachedProjectURL{}, gate: map[string]chan struct{}{}, now: time.Now, slots: make(chan struct{}, options.MaxConcurrentLookups)}
}

// ThreadURL returns a validated thread link, resolving the project at most once
// concurrently and caching only successful results.
func (resolver *URLResolver) ThreadURL(ctx context.Context, project, threadID string) (string, error) {
	resolver.requireInitialized()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validProject(project, false) || !validThreadID(threadID) {
		return "", ErrProjectURL
	}
	for {
		if projectURL, ok := resolver.cached(project); ok {
			return assembleThreadURL(projectURL, threadID, resolver.options.MaxURLBytes)
		}
		resolver.mu.Lock()
		if pending := resolver.gate[project]; pending != nil {
			resolver.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-pending:
				continue
			}
		}
		pending := make(chan struct{})
		resolver.gate[project] = pending
		resolver.mu.Unlock()

		projectURL, err := resolver.lookupProject(ctx, project)
		resolver.mu.Lock()
		delete(resolver.gate, project)
		if err == nil {
			resolver.evictIfNeededLocked()
			resolver.cache[project] = cachedProjectURL{url: projectURL, expiresAt: resolver.now().Add(resolver.options.CacheTTL)}
		}
		close(pending)
		resolver.mu.Unlock()
		if err != nil {
			return "", err
		}
		return assembleThreadURL(projectURL, threadID, resolver.options.MaxURLBytes)
	}
}

// CachedThreadURL is a non-blocking lookup for transient user interfaces.
func (resolver *URLResolver) CachedThreadURL(project, threadID string) (string, bool) {
	resolver.requireInitialized()
	if !validProject(project, false) || !validThreadID(threadID) {
		return "", false
	}
	projectURL, ok := resolver.cached(project)
	if !ok {
		return "", false
	}
	result, err := assembleThreadURL(projectURL, threadID, resolver.options.MaxURLBytes)
	return result, err == nil
}

func (resolver *URLResolver) lookupProject(ctx context.Context, project string) (string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, resolver.options.LookupTimeout)
	defer cancel()
	select {
	case resolver.slots <- struct{}{}:
	case <-lookupCtx.Done():
		return "", lookupCtx.Err()
	}
	type lookupResult struct {
		url string
		err error
	}
	result := make(chan lookupResult, 1)
	go func() {
		defer func() { <-resolver.slots }()
		raw, err := callProjectLookup(lookupCtx, resolver.lookup, project)
		result <- lookupResult{url: raw, err: err}
	}()
	var raw string
	var err error
	select {
	case <-lookupCtx.Done():
		return "", lookupCtx.Err()
	case completed := <-result:
		raw, err = completed.url, completed.err
	}
	if err != nil {
		if lookupCtx.Err() != nil {
			return "", lookupCtx.Err()
		}
		return "", ErrProjectLookup
	}
	if len(raw) == 0 || len(raw) > resolver.options.MaxURLBytes {
		return "", ErrProjectURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrProjectURL
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func callProjectLookup(ctx context.Context, lookup ProjectLookup, project string) (projectURL string, err error) {
	defer func() {
		if recover() != nil {
			projectURL, err = "", ErrProjectLookup
		}
	}()
	return lookup.ProjectURL(ctx, project)
}

func (resolver *URLResolver) cached(project string) (string, bool) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	entry, ok := resolver.cache[project]
	if !ok {
		return "", false
	}
	if !entry.expiresAt.After(resolver.now()) {
		delete(resolver.cache, project)
		return "", false
	}
	return entry.url, true
}

func (resolver *URLResolver) evictIfNeededLocked() {
	if len(resolver.cache) < resolver.options.MaxEntries {
		return
	}
	type candidate struct {
		project string
		expires time.Time
	}
	candidates := make([]candidate, 0, len(resolver.cache))
	for project, entry := range resolver.cache {
		candidates = append(candidates, candidate{project: project, expires: entry.expiresAt})
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].expires.Equal(candidates[right].expires) {
			return candidates[left].project < candidates[right].project
		}
		return candidates[left].expires.Before(candidates[right].expires)
	})
	delete(resolver.cache, candidates[0].project)
}

func assembleThreadURL(projectURL, threadID string, maximum int) (string, error) {
	parsed, err := url.Parse(projectURL)
	if err != nil {
		return "", ErrProjectURL
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/t/" + url.PathEscape(threadID)
	query := parsed.Query()
	query.Set("utm_source", "dago")
	parsed.RawQuery = query.Encode()
	result := parsed.String()
	if len(result) > maximum {
		return "", ErrProjectURL
	}
	return result, nil
}

func validThreadID(value string) bool {
	return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "/?#") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func (resolver *URLResolver) requireInitialized() {
	if resolver == nil || nilValue(resolver.lookup) || resolver.cache == nil || resolver.gate == nil || resolver.now == nil || resolver.slots == nil {
		panic("initialized trace URL resolver is required")
	}
}
