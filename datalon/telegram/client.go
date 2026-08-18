package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

var (
	ErrAPIResponse   = errors.New("telegram Bot API response is invalid")
	ErrTransport     = errors.New("telegram Bot API transport failed")
	ErrPayloadTooBig = errors.New("telegram Bot API payload exceeds the configured limit")
	ErrInvalidUpdate = errors.New("telegram update is invalid")
)

// HTTPClient is the caller-owned HTTP boundary used for every Bot API call.
// Implementations must honor request contexts.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// APIError is a sanitized Bot API failure. It never includes the credentialed
// request URL or raw response body.
type APIError struct {
	Method      string
	Status      int
	Description string
	RetryAfter  time.Duration
}

func (err *APIError) Error() string {
	if err.Description == "" {
		return fmt.Sprintf("telegram Bot API %s failed with status %d", err.Method, err.Status)
	}
	return fmt.Sprintf("telegram Bot API %s failed: %s", err.Method, err.Description)
}

func (err *APIError) Retryable() bool {
	return err.Status == http.StatusTooManyRequests || err.Status >= 500
}

type apiEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter float64 `json:"retry_after"`
	} `json:"parameters"`
	RetryAfter float64 `json:"retry_after"`
}

func (channel *Channel) call(ctx context.Context, method string, params any, target any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%w: encode %s request", ErrAPIResponse, method)
	}
	if int64(len(body)) > channel.options.MaxRequestBytes {
		return ErrPayloadTooBig
	}
	callCtx, cancel := context.WithTimeout(ctx, channel.options.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		callCtx,
		http.MethodPost,
		channel.options.APIBase+"/bot"+channel.token+"/"+method,
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("%w: construct %s request", ErrTransport, method)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := channel.client.Do(request)
	if err != nil {
		if callCtx.Err() != nil {
			return callCtx.Err()
		}
		return fmt.Errorf("%w: %s", ErrTransport, method)
	}
	if response == nil || response.Body == nil {
		return fmt.Errorf("%w: %s returned an empty response", ErrTransport, method)
	}
	defer response.Body.Close()
	payload, err := readBounded(response.Body, channel.options.MaxResponseBytes)
	if err != nil {
		return err
	}
	var envelope apiEnvelope
	if json.Unmarshal(payload, &envelope) != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return &APIError{Method: method, Status: response.StatusCode}
		}
		return fmt.Errorf("%w: decode %s response", ErrAPIResponse, method)
	}
	if !envelope.OK || response.StatusCode < 200 || response.StatusCode >= 300 {
		description := boundedText(envelope.Description, channel.options.MaxErrorBytes)
		retryAfter := envelope.Parameters.RetryAfter
		if retryAfter == 0 {
			retryAfter = envelope.RetryAfter
		}
		return &APIError{
			Method: method, Status: response.StatusCode, Description: description,
			RetryAfter: boundedRetryAfter(retryAfter, channel.options.MaxRetryDelay),
		}
	}
	if target == nil {
		return nil
	}
	if len(envelope.Result) == 0 || json.Unmarshal(envelope.Result, target) != nil {
		return fmt.Errorf("%w: decode %s result", ErrAPIResponse, method)
	}
	return nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", ErrTransport)
	}
	if int64(len(payload)) > limit {
		return nil, ErrPayloadTooBig
	}
	return payload, nil
}

func boundedRetryAfter(seconds float64, limit time.Duration) time.Duration {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0
	}
	if seconds >= float64(limit)/float64(time.Second) {
		return limit
	}
	value := time.Duration(seconds * float64(time.Second))
	return value
}

func boundedText(value string, limit int) string {
	runes := []rune(value)
	for index, value := range runes {
		if value < 0x20 || value == 0x7f {
			runes[index] = ' '
		}
	}
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
