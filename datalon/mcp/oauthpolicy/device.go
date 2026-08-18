package oauthpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type deviceResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (service *Service) githubTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	service.mu.Lock()
	if service.token != nil {
		token := service.token
		service.mu.Unlock()
		return oauth2.StaticTokenSource(token), nil
	}
	service.mu.Unlock()
	stored, err := service.store.Load(ctx, service.key)
	if err != nil {
		return nil, sanitizeContext(ctx, ErrTokenStore)
	}
	if stored == nil {
		return nil, nil
	}
	service.mu.Lock()
	service.token = stored
	service.mu.Unlock()
	return oauth2.StaticTokenSource(stored), nil
}

func (service *Service) authorizeGitHub(ctx context.Context) error {
	presenter, ok := service.interaction.(DeviceCodePresenter)
	if !ok {
		return errors.New("GitHub OAuth requires a device-code interaction")
	}
	device, err := service.requestDeviceCode(ctx)
	if err != nil {
		return err
	}
	if err := presenter.PresentDeviceCode(ctx, DeviceCode{
		UserCode: device.UserCode, VerificationURI: device.VerificationURI,
		ExpiresIn: time.Duration(device.ExpiresIn) * time.Second,
	}); err != nil {
		return sanitizeContext(ctx, ErrInteraction)
	}
	token, err := service.pollDeviceToken(ctx, device)
	if err != nil {
		return err
	}
	if err := service.store.Save(ctx, service.key, token); err != nil {
		return sanitizeContext(ctx, ErrTokenStore)
	}
	service.mu.Lock()
	service.token = token
	service.mu.Unlock()
	return nil
}

func (service *Service) requestDeviceCode(ctx context.Context) (deviceResponse, error) {
	response, err := service.postForm(ctx, githubDeviceURL, url.Values{"client_id": {githubClientID}})
	if err != nil {
		return deviceResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return deviceResponse{}, ErrDeviceCode
	}
	var payload deviceResponse
	if err := decodeJSON(response.Body, &payload); err != nil {
		return deviceResponse{}, ErrInvalidResponse
	}
	if !validDeviceResponse(payload, service.limits) {
		return deviceResponse{}, ErrInvalidResponse
	}
	return payload, nil
}

func validDeviceResponse(response deviceResponse, limits limits) bool {
	maximumSeconds := int64(limits.maxDeviceLifetime / time.Second)
	if response.DeviceCode == "" || len(response.DeviceCode) > limits.maxTokenBytes ||
		response.UserCode == "" || len(response.UserCode) > 256 || strings.ContainsAny(response.UserCode, "\x00\r\n") ||
		response.ExpiresIn <= 0 || response.ExpiresIn > maximumSeconds || response.Interval < 0 ||
		response.Interval > int64(maximumPollDelay/time.Second) {
		return false
	}
	parsed, err := url.Parse(response.VerificationURI)
	return err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Host, "github.com") &&
		parsed.User == nil && parsed.Fragment == "" && parsed.Path != ""
}

func (service *Service) pollDeviceToken(ctx context.Context, device deviceResponse) (*oauth2.Token, error) {
	interval := time.Duration(device.Interval) * time.Second
	if interval < service.limits.minPollInterval {
		interval = service.limits.minPollInterval
	}
	if interval > service.limits.maxPollInterval {
		interval = service.limits.maxPollInterval
	}
	deadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	for poll := 0; poll < service.limits.maxPolls; poll++ {
		if time.Now().After(deadline) {
			return nil, ErrDeviceCode
		}
		if err := service.wait(ctx, interval); err != nil {
			return nil, err
		}
		response, err := service.postForm(ctx, githubTokenURL, url.Values{
			"client_id": {githubClientID}, "device_code": {device.DeviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		})
		if err != nil {
			return nil, err
		}
		var payload tokenResponse
		decodeErr := decodeJSON(response.Body, &payload)
		_ = response.Body.Close()
		if decodeErr != nil {
			return nil, ErrInvalidResponse
		}
		if response.StatusCode != http.StatusOK && payload.Error == "" {
			return nil, ErrDeviceCode
		}
		switch payload.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			if interval > service.limits.maxPollInterval {
				interval = service.limits.maxPollInterval
			}
			continue
		case "access_denied", "expired_token":
			return nil, ErrAuthorizationDenied
		case "":
		default:
			return nil, ErrDeviceCode
		}
		token := &oauth2.Token{
			AccessToken: payload.AccessToken, TokenType: payload.TokenType,
			RefreshToken: payload.RefreshToken,
		}
		if payload.ExpiresIn > 0 {
			if payload.ExpiresIn > int64((365*24*time.Hour)/time.Second) {
				return nil, ErrInvalidResponse
			}
			token.Expiry = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
		}
		if !validToken(token, service.limits.maxTokenBytes) {
			return nil, ErrInvalidResponse
		}
		return token, nil
	}
	return nil, ErrDeviceCode
}

func (service *Service) postForm(ctx context.Context, endpoint string, values url.Values) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		panic("invalid static OAuth endpoint")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := service.client.Do(request)
	if err != nil {
		return nil, sanitizeContext(ctx, ErrDeviceCode)
	}
	return response, nil
}

func decodeJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrInvalidResponse
	}
	return nil
}
