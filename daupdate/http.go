package daupdate

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const defaultHTTPTimeout = 30 * time.Second

// HTTPSource retrieves a signed channel manifest and same-origin artifacts.
// The caller-owned transport retains DNS, TLS, certificate, and IP policy.
type HTTPSource struct {
	client *http.Client
	base   *url.URL
	origin string
}

// NewHTTPSource constructs a source rooted at an explicit HTTPS release
// directory. It performs no request. Static invalid inputs panic.
func NewHTTPSource(client *http.Client, manifestBaseURL string) *HTTPSource {
	if client == nil {
		panic("update HTTP client is required")
	}
	base, err := url.Parse(strings.TrimSpace(manifestBaseURL))
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		panic("update manifest base URL must be an HTTPS directory origin")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copyClient.Timeout == 0 || copyClient.Timeout > defaultHTTPTimeout {
		copyClient.Timeout = defaultHTTPTimeout
	}
	return &HTTPSource{client: &copyClient, base: base, origin: base.Scheme + "://" + base.Host}
}

func (source *HTTPSource) FetchManifest(ctx context.Context, channel string, maxBytes int64) (io.ReadCloser, error) {
	if !channelPattern.MatchString(channel) {
		return nil, ErrUpdateCheckFailed
	}
	target := *source.base
	target.Path = path.Join(source.base.Path, channel+".json")
	return source.fetch(ctx, target.String(), maxBytes)
}

func (source *HTTPSource) FetchArtifact(ctx context.Context, value string, maxBytes int64) (io.ReadCloser, error) {
	target, err := url.Parse(value)
	if err != nil || target.Scheme+"://"+target.Host != source.origin || !validArtifactURL(value) {
		return nil, ErrUpdateCheckFailed
	}
	return source.fetch(ctx, value, maxBytes)
}

func (source *HTTPSource) fetch(ctx context.Context, value string, maxBytes int64) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, value, nil)
	if err != nil {
		return nil, ErrUpdateCheckFailed
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := source.client.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrUpdateCheckFailed
	}
	if response.StatusCode != http.StatusOK || response.ContentLength > maxBytes {
		response.Body.Close()
		return nil, ErrUpdateCheckFailed
	}
	return response.Body, nil
}

var _ Source = (*HTTPSource)(nil)
