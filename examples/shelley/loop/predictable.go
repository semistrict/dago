package loop

import (
	"context"
	"io"
	"iter"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/semistrict/dago/damodel"
)

// Constants for the inline-image fixture. The model first writes a small PNG
// into the conversation working directory, then references it with a relative
// markdown path so the UI exercises its message-file route.
const (
	inlineImagePath      = "shelley-inline-image-demo.png"
	inlineImageSentinel  = "SHELLEY_INLINE_IMAGE_DEMO"
	inlineImagePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAADAAAAAwCAIAAADYYG7QAAAAS0lEQVR42u3OMQ0AIAwAsMlBxEQgBznImQiUcGFiH00qoHEyW+Q+LUJISEhISEhISEhISOiz0K3RYtZqISQkJCQkJCQkJCQk9FnoAQiSrlPnJLTeAAAAAElFTkSuQmCC"
)

// The screenshot fixture deliberately lives outside the conversation working
// directory. This holds down the server route that serves browser screenshots.
const (
	screenshotImageDir      = "/tmp/shelley-screenshots"
	screenshotImagePath     = screenshotImageDir + "/shelley-screenshot-demo.png"
	screenshotImageSentinel = "SHELLEY_SCREENSHOT_IMAGE_DEMO"
)

// PredictableService is a native dago chat model used by Shelley's tests and
// demos. Its supported prompts are implemented in invokeNative.
type PredictableService struct {
	tokenContextWindow int
	mu                 sync.Mutex
	recentRequests     []damodel.Request
	responseDelay      time.Duration
}

// NewPredictableService creates the deterministic native chat fixture.
func NewPredictableService() *PredictableService {
	service := &PredictableService{tokenContextWindow: 200000}
	if value := os.Getenv("PREDICTABLE_DELAY_MS"); value != "" {
		if milliseconds, err := strconv.Atoi(value); err == nil && milliseconds > 0 {
			service.responseDelay = time.Duration(milliseconds) * time.Millisecond
		}
	}
	return service
}

func (service *PredictableService) Profile() damodel.Profile {
	return damodel.Profile{
		Provider: "builtin", Model: "predictable-v1", ContextWindow: service.tokenContextWindow,
		MaxOutputTokens: 8192, ToolCalling: true, ParallelToolCalls: false,
		SupportsReasoning: true,
		SupportsImages:    true, MaxImageDimension: 2000, MaxImageBytes: 5 * 1024 * 1024,
	}
}

func (service *PredictableService) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	return service.invokeNative(ctx, request)
}

func (service *PredictableService) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	response, err := service.Invoke(ctx, request)
	if err != nil {
		return nil, err
	}
	return &predictableStream{ctx: ctx, chunk: damodel.Chunk{
		MessageDelta: response.Message,
		Structured:   response.Structured,
		Done:         true,
	}}, nil
}

type predictableStream struct {
	ctx   context.Context
	chunk damodel.Chunk
	done  bool
}

func (stream *predictableStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *predictableStream) Next(ctx context.Context) (damodel.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return damodel.Chunk{}, err
	}
	if stream.done {
		return damodel.Chunk{}, io.EOF
	}
	stream.done = true
	return stream.chunk, nil
}

func (*predictableStream) Close() error { return nil }

// GetRecentRequests returns a snapshot of the most recent native requests.
func (service *PredictableService) GetRecentRequests() []damodel.Request {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]damodel.Request(nil), service.recentRequests...)
}

// GetLastRequest returns the most recent native request, or nil if none exists.
func (service *PredictableService) GetLastRequest() *damodel.Request {
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.recentRequests) == 0 {
		return nil
	}
	request := service.recentRequests[len(service.recentRequests)-1]
	return &request
}

// SetResponseDelay configures a deterministic delay before every invocation.
func (service *PredictableService) SetResponseDelay(delay time.Duration) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.responseDelay = delay
}

// ClearRequests clears the recorded native request history.
func (service *PredictableService) ClearRequests() {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.recentRequests = nil
}

const wideTablesMarkdown = `Here are some wide tables to test rendering:

## Narrow Table (should look fine)

| Name | Age | City |
|------|-----|------|
| Alice | 30 | NYC |
| Bob | 25 | LA |

## Wide Table (many columns)

| ID | First Name | Last Name | Email Address | Phone Number | Street Address | City | State | Zip Code | Country | Department | Job Title | Start Date | Salary | Manager |
|----|-----------|-----------|--------------|-------------|---------------|------|-------|----------|---------|-----------|-----------|-----------|--------|--------|
| 1 | Alexander | Montgomery | alexander.montgomery@longcompanyname.com | +1-555-0123 | 1234 Willowbrook Lane | San Francisco | California | 94102 | United States | Engineering | Senior Staff Engineer | 2019-03-15 | $185,000 | Sarah Johnson |
| 2 | Elizabeth | Fitzgerald | elizabeth.fitzgerald@longcompanyname.com | +1-555-0456 | 5678 Meadowridge Drive | New York | New York | 10001 | United States | Product Management | Director of Product | 2018-07-22 | $210,000 | Michael Chen |
| 3 | Christopher | Worthington | christopher.worthington@longcompanyname.com | +1-555-0789 | 9012 Thunderbird Road | Chicago | Illinois | 60601 | United States | Data Science | Principal Data Scientist | 2020-01-10 | $195,000 | Sarah Johnson |

## Table with Code and Long Content

| Function | Signature | Description | Example Usage | Return Type |
|----------|-----------|-------------|---------------|-------------|
| ` + "`processDataPipeline`" + ` | ` + "`func processDataPipeline(ctx context.Context, input []DataRecord, opts ...ProcessOption) (*PipelineResult, error)`" + ` | Processes a batch of data records through the configured pipeline stages | ` + "`result, err := processDataPipeline(ctx, records, WithParallelism(4), WithTimeout(30*time.Second))`" + ` | ` + "`*PipelineResult`" + ` |
| ` + "`validateConfiguration`" + ` | ` + "`func validateConfiguration(cfg *Config, validators ...ConfigValidator) ([]ValidationError, error)`" + ` | Validates the configuration against all registered validators | ` + "`errs, err := validateConfiguration(cfg, RequiredFieldsValidator{}, RangeValidator{})`" + ` | ` + "`[]ValidationError`" + ` |

## Table with Long Headers

| Configuration Parameter Name | Default Value | Minimum Allowed Value | Maximum Allowed Value | Environment Variable Override | Description of Behavior |
|------------------------------|---------------|----------------------|----------------------|------------------------------|-------------------------|
| max_concurrent_connections | 100 | 1 | 10000 | APP_MAX_CONNECTIONS | Limits simultaneous connections |
| request_timeout_seconds | 30 | 1 | 300 | APP_REQUEST_TIMEOUT | Per-request timeout |
| background_worker_pool_size | 4 | 1 | 64 | APP_WORKER_POOL | Number of background workers |

## Numeric Data Table

| Metric | Q1 2024 | Q2 2024 | Q3 2024 | Q4 2024 | YoY Change | Trend |
|--------|---------|---------|---------|---------|------------|-------|
| Revenue ($M) | 12.45 | 13.82 | 15.01 | 16.73 | +34.4% | 📈 |
| Active Users | 1,234,567 | 1,456,789 | 1,678,901 | 1,890,123 | +53.2% | 📈 |
| Churn Rate | 4.2% | 3.8% | 3.5% | 3.1% | -26.2% | 📉 |
| NPS Score | 42 | 45 | 48 | 52 | +23.8% | 📈 |

That's a variety of table widths for testing!`
