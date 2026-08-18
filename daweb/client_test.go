package daweb

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (function resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return function(ctx, network, host)
}

type pointerResolver struct{}

func (*pointerResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, nil
}

func fixedResolver(addresses ...string) Resolver {
	return resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		result := make([]netip.Addr, len(addresses))
		for index, address := range addresses {
			result[index] = netip.MustParseAddr(address)
		}
		return result, nil
	})
}

func hostResolver(values map[string][]string, calls *[]string) Resolver {
	return resolverFunc(func(_ context.Context, _, host string) ([]netip.Addr, error) {
		*calls = append(*calls, host)
		addresses := values[host]
		result := make([]netip.Addr, len(addresses))
		for index, address := range addresses {
			result[index] = netip.MustParseAddr(address)
		}
		return result, nil
	})
}

func response(request *http.Request, status int, body string, headers map[string]string) *http.Response {
	header := make(http.Header, len(headers))
	for name, value := range headers {
		header.Set(name, value)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestNewClientDefaultsAndStaticValidation(t *testing.T) {
	client := NewClient(Options{})
	config := client.configured()
	if config.timeout != 30*time.Second || config.maxTimeout != 60*time.Second || config.maxRedirects != 5 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	if config.maxResponseBytes != 2<<20 || config.maxRequestBytes != 256<<10 || config.maxRenderedBytes != 256<<10 {
		t.Fatalf("unexpected byte defaults: %+v", config)
	}
	if bounded := NewClient(Options{MaxTimeout: time.Second}).configured(); bounded.timeout != time.Second {
		t.Fatalf("default timeout should honor a smaller maximum: %+v", bounded)
	}
	var zero Client
	if zero.configured().resolver == nil {
		t.Fatal("zero Client should install a default resolver")
	}
	var typedNil *pointerResolver
	if configured := NewClient(Options{Resolver: typedNil}).configured(); configured.resolver == nil {
		t.Fatal("typed nil optional resolver should select the default resolver")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("negative static limits must panic")
		}
	}()
	NewClient(Options{MaxResponseBytes: -1})
}

func TestValidateTargetRejectsPrivateReservedAndWrappedAddresses(t *testing.T) {
	addresses := []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "192.168.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1",
		"203.0.113.1", "224.0.0.1", "240.0.0.1", "::", "::1", "fc00::1", "fe80::1",
		"ff00::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "2002:a9fe:a9fe::1",
		"::192.168.0.1", "64:ff9b::192.168.0.1", "64:ff9b:1::c0a8:1",
	}
	for _, address := range addresses {
		t.Run(address, func(t *testing.T) {
			_, err := validateTarget(context.Background(), fixedResolver(address), "https://target.example/path")
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("validate %s error = %v, want ErrBlockedAddress", address, err)
			}
		})
	}
}

func TestValidateTargetRejectsLiteralAndAlternateLoopbackForms(t *testing.T) {
	for _, test := range []struct {
		url      string
		resolved string
	}{
		{url: "http://127.0.0.1/"},
		{url: "http://[::ffff:127.0.0.1]/"},
		{url: "http://2130706433/", resolved: "127.0.0.1"},
		{url: "http://0x7f000001/", resolved: "127.0.0.1"},
		{url: "http://0177.0.0.1/", resolved: "127.0.0.1"},
	} {
		t.Run(test.url, func(t *testing.T) {
			resolver := fixedResolver("8.8.8.8")
			if test.resolved != "" {
				resolver = fixedResolver(test.resolved)
			}
			_, err := validateTarget(context.Background(), resolver, test.url)
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("error = %v, want ErrBlockedAddress", err)
			}
		})
	}
}

func TestValidateTargetRejectsMixedDNSCredentialsSchemesAndPorts(t *testing.T) {
	_, err := validateTarget(context.Background(), fixedResolver("8.8.8.8", "10.0.0.1"), "https://mixed.example/")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("mixed DNS error = %v", err)
	}
	for _, rawURL := range []string{
		"file:///etc/passwd", "gopher://example.test/", "http:///missing",
		"http://user:" + "pass@example.test/", "http://localhost/", "http://service.localhost/",
		"http://example.test:0/", "http://example.test:65536/", "http://example.test:bad/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, validationErr := validateTarget(context.Background(), fixedResolver("8.8.8.8"), rawURL)
			if validationErr == nil {
				t.Fatal("expected URL rejection")
			}
		})
	}
}

func TestValidateTargetAllowsPublicDNSAndExplicitPort(t *testing.T) {
	target, err := validateTarget(context.Background(), fixedResolver("8.8.8.8", "2001:4860:4860::8888"), "https://Example.Test.:8443/a#fragment")
	if err != nil {
		t.Fatal(err)
	}
	if target.host != "example.test" || target.url.Host != "example.test:8443" || target.url.Fragment != "" {
		t.Fatalf("target = %+v", target)
	}
	if len(target.ips) != 2 {
		t.Fatalf("addresses = %v", target.ips)
	}
}

func TestPinnedDialUsesOnlyValidatedAddressesAndRequestedPort(t *testing.T) {
	target := validatedTarget{host: "public.example", ips: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}}
	var addresses []string
	want := errors.New("dial failed")
	dial := pinnedDialContext(func(_ context.Context, _, address string) (net.Conn, error) {
		addresses = append(addresses, address)
		return nil, want
	}, target)
	_, err := dial(context.Background(), "tcp", "public.example:8443")
	if err == nil || len(addresses) != 2 || addresses[0] != "8.8.8.8:8443" || addresses[1] != "1.1.1.1:8443" {
		t.Fatalf("addresses=%v err=%v", addresses, err)
	}
	if _, err = dial(context.Background(), "tcp", "attacker.example:8443"); err == nil || !strings.Contains(err.Error(), "unexpected hostname") {
		t.Fatalf("unexpected-host error = %v", err)
	}
}

func TestRedirectsRevalidateEveryHopAndBlockPrivateTarget(t *testing.T) {
	var resolved []string
	client := NewClient(Options{Resolver: hostResolver(map[string][]string{
		"public.example":   {"8.8.8.8"},
		"internal.example": {"169.254.169.254"},
	}, &resolved)})
	requests := 0
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		requests++
		return response(request, http.StatusFound, "", map[string]string{"Location": "http://internal.example/secret"}), nil
	}
	_, err := client.Do(context.Background(), "https://public.example/start", Request{})
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("error = %v", err)
	}
	if requests != 1 || strings.Join(resolved, ",") != "public.example,internal.example" {
		t.Fatalf("requests=%d resolved=%v", requests, resolved)
	}
}

func TestRedirectStripsCredentialsAcrossOrigins(t *testing.T) {
	var resolved []string
	client := NewClient(Options{Resolver: hostResolver(map[string][]string{
		"one.example": {"8.8.8.8"}, "two.example": {"1.1.1.1"},
	}, &resolved)})
	var captured []*http.Request
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		captured = append(captured, request.Clone(request.Context()))
		if len(captured) == 1 {
			return response(request, http.StatusTemporaryRedirect, "", map[string]string{"Location": "https://two.example/end"}), nil
		}
		return response(request, http.StatusOK, "done", nil), nil
	}
	result, err := client.Do(context.Background(), "https://one.example/start", Request{
		Headers: map[string]string{"Authorization": "Bearer secret", "Cookie": "session=secret", "X-Safe": "yes"},
	})
	if err != nil || result.Body != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if captured[1].Header.Get("Authorization") != "" || captured[1].Header.Get("Cookie") != "" || captured[1].Header.Get("X-Safe") != "" {
		t.Fatalf("redirect headers = %v", captured[1].Header)
	}
}

func TestRedirectRefusesCrossOriginRequestBody(t *testing.T) {
	var resolved []string
	client := NewClient(Options{Resolver: hostResolver(map[string][]string{
		"one.example": {"8.8.8.8"}, "two.example": {"1.1.1.1"},
	}, &resolved)})
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		return response(request, http.StatusTemporaryRedirect, "", map[string]string{"Location": "https://two.example/end"}), nil
	}
	_, err := client.Do(context.Background(), "https://one.example/start", Request{Method: http.MethodPost, Body: "secret material"})
	if !errors.Is(err, ErrInvalidURL) || !strings.Contains(err.Error(), "request body") {
		t.Fatalf("error = %v", err)
	}
}

func TestRedirectCapAndMalformedRedirect(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8"), MaxRedirects: 2})
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		return response(request, http.StatusFound, "", map[string]string{"Location": "/again"}), nil
	}
	_, err := client.Do(context.Background(), "https://public.example/start", Request{})
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("error = %v", err)
	}
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		return response(request, http.StatusFound, "", nil), nil
	}
	_, err = client.Do(context.Background(), "https://public.example/start", Request{})
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("missing Location error = %v", err)
	}
}

func TestRequestBoundsBodyResponseHeadersAndTimeout(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8"), Timeout: 500 * time.Millisecond, MaxRequestBytes: 128, MaxResponseBytes: 4, MaxTimeout: time.Second})
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		return response(request, http.StatusOK, "12345", nil), nil
	}
	if _, err := client.Do(context.Background(), "https://public.example", Request{Method: http.MethodPost, Body: strings.Repeat("x", 129)}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("request bound error = %v", err)
	}
	if _, err := client.Do(context.Background(), "https://public.example", Request{}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("response bound error = %v", err)
	}
	if _, err := client.Do(context.Background(), "https://public.example", Request{Headers: map[string]string{"Host": "internal"}}); err == nil {
		t.Fatal("Host header should be rejected")
	}
	if _, err := client.Do(context.Background(), "https://public.example", Request{Timeout: 2 * time.Second}); err == nil {
		t.Fatal("timeout above maximum should be rejected")
	}
}

func TestCancellationStopsDNSResolution(t *testing.T) {
	resolver := resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client := NewClient(Options{Resolver: resolver})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Do(ctx, "https://public.example", Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
