package dacode

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/semistrict/dago/datalon/tracing"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const maxTraceNoticeRunes = 2048
const defaultTraceLookupTimeout = 2 * time.Second

type traceThreadURLResolver interface {
	ThreadURL(context.Context, string, string) (string, error)
}

type traceCommandRequest struct {
	Project     string
	ThreadID    string
	HasMessages bool
}

type traceCommandResult struct {
	URL     string
	Message string
}

// traceCommand resolves links without opening a browser or mutating the
// transcript. A nil resolver is a useful unconfigured state.
type traceCommand struct {
	resolver traceThreadURLResolver
}

func newTraceCommand(resolver traceThreadURLResolver) *traceCommand {
	return &traceCommand{resolver: resolver}
}

func (command *traceCommand) resolve(ctx context.Context, request traceCommandRequest) traceCommandResult {
	if command == nil {
		panic("dacode: trace command is required")
	}
	if ctx == nil {
		panic("dacode: trace context is required")
	}
	if err := ctx.Err(); err != nil {
		return traceCommandResult{Message: "Trace lookup was cancelled."}
	}
	project := boundedTraceValue(request.Project, 256)
	threadID := boundedTraceValue(request.ThreadID, 512)
	if threadID == "" {
		return traceCommandResult{Message: "No active session."}
	}
	if command.resolver == nil || project == "" {
		return traceCommandResult{Message: "LangSmith tracing is not configured. Run /auth and select LangSmith to enable tracing."}
	}
	lookupContext := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		lookupContext, cancel = context.WithTimeout(ctx, defaultTraceLookupTimeout)
	}
	defer cancel()
	link, err := callTraceResolver(lookupContext, command.resolver, project, threadID)
	if err != nil {
		return traceCommandResult{Message: traceLookupFailure(lookupContext, err)}
	}
	if !validTraceLink(link) {
		return traceCommandResult{Message: "Failed to resolve LangSmith thread URL."}
	}
	message := fmt.Sprintf("Opening tracing project %q in the default browser:\n%s", project, link)
	if !request.HasMessages {
		message += "\n\nThe trace will be empty until you send the first message in this thread."
	}
	return traceCommandResult{URL: link, Message: boundedTraceNotice(message)}
}

func callTraceResolver(ctx context.Context, resolver traceThreadURLResolver, project, threadID string) (link string, err error) {
	defer func() {
		if recover() != nil {
			link, err = "", tracing.ErrProjectLookup
		}
	}()
	return resolver.ThreadURL(ctx, project, threadID)
}

func traceLookupFailure(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "Could not reach LangSmith to resolve the thread URL. Check your network connection and try again."
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return "Trace lookup was cancelled."
	}
	if errors.Is(err, tracing.ErrProjectLookup) {
		return "LangSmith rejected or failed the project lookup. Verify the stored credential and project name."
	}
	return "Failed to resolve LangSmith thread URL."
}

func validTraceLink(value string) bool {
	if value == "" || len(value) > 16<<10 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == ""
}

func boundedTraceValue(value string, limit int) string {
	value = strings.TrimSpace(unicodesecurity.RenderTerminalSafe(value))
	value = strings.ReplaceAll(value, "\n", " ")
	runes := []rune(value)
	if len(runes) > limit {
		return ""
	}
	return value
}

func boundedTraceNotice(value string) string {
	value = unicodesecurity.RenderTerminalSafe(value)
	runes := []rune(value)
	if len(runes) <= maxTraceNoticeRunes {
		return value
	}
	return string(runes[:maxTraceNoticeRunes-3]) + "..."
}
