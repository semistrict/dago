package dago

import (
	"context"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

const (
	fireworksGLM52Provider = "fireworks"
	fireworksGLM52Model    = "accounts/fireworks/models/glm-5p2"

	glm52ExecutionPrompt = `<execution>
Execute the task directly. Identify every required output path before acting, and translate all must, only, exact, ordered, ranged, and prohibited requirements into a short execution checklist. Prefer concrete progress over commentary.

Create a valid, parseable artifact before long-running installs, research, or tuning. Keep it valid while refining it, reserve the final part of the run for verification, and near the time limit stop exploring and leave the best complete artifact.

Use the exact requested version, date, revision, tokenizer, library, or source, never memory or a nearby substitute. Before changing protected inputs, record a checksum and use a separate working copy when the task permits. Treat supplied or fetched source-of-truth data as authoritative: apply only transformations the task explicitly requests, preserve everything else exactly, and compare the final artifact against that source or its stated allowlist. Do not strip prefixes or tags, repair grammar, normalize, reformat, or add cleanup unless explicitly requested.

Run the artifact with the same interpreter and entrypoint that will be evaluated, and confirm dependencies through that interpreter. A successful exit only proves that the command ran; checks must assert the actual result against the required value and fail nonzero on mismatch. Exercise task-stated examples plus relevant values below, at, and above each boundary, including negative cases and cleanup behavior.

For optimization tasks, preserve correctness and input bytes first. Inspect algorithm and execution-plan structure instead of relying only on timing, then use repeated measurements against the requested reference with enough margin for noise.

This is a text-only model. Do not call ` + "`read_file`" + ` on images, PDFs, audio, or video. Extract needed text, metadata, or frames with a shell utility or script. Never place binary or encoded media in model context. Do not reopen generated media for visual inspection; validate it with task-specific non-visual checks.

If a required dependency or command is unavailable, make one retry after correcting the invocation. If it still fails, pivot immediately instead of repeating the same approach.

Fix only failures caused by your work. Stop immediately once every requested artifact is complete and the assertions pass; do not add speculative extras.
</execution>`

	glm52StallRecoveryPrompt = `<terminal_stall_recovery>
Your prior attempt exhausted its output budget without taking an action. Stop explaining or planning and call a tool now to create or update the requested deliverable. Prefer the smallest valid artifact, then run one discriminating check. Keep any reasoning brief enough to reach the tool call.
</terminal_stall_recovery>`
)

func glm52HarnessPrompt(provider, model string) (string, bool) {
	switch provider + ":" + model {
	case "fireworks:accounts/fireworks/models/glm-5p2",
		"openrouter:z-ai/glm-5.2",
		"baseten:zai-org/GLM-5.2":
		return glm52ExecutionPrompt, true
	default:
		return "", false
	}
}

// GLM52TerminalStallRecovery returns the one-shot recovery middleware for a
// headless harness. Callers should not install it in interactive agents, whose
// tool-free responses can be intentional.
//
// Recovery is deliberately limited to the measured Fireworks GLM-5.2 model. A
// normalized max-token response containing exactly one assistant message, no
// tool call, and no structured result is retried once with reasoning disabled
// and a required tool choice. All other models and response shapes pass through.
func GLM52TerminalStallRecovery() dagent.Middleware {
	return dagent.Middleware{
		Name: "glm_5p2_terminal_stall_recovery",
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			response, err := next(ctx, request)
			if err != nil || !isFireworksGLM52(request.Model) || !isGLM52TerminalStall(response) {
				return response, err
			}
			if err := ctx.Err(); err != nil {
				return dagent.ModelResponse{}, err
			}
			retry := request.Clone()
			retry.SystemMessage = appendGLM52RecoveryPrompt(retry.SystemMessage)
			retry.ToolChoice = &damodel.ToolChoice{Mode: "required"}
			if retry.Reasoning == nil {
				retry.Reasoning = &damodel.Reasoning{Effort: "none"}
			} else {
				retry.Reasoning.Effort = "none"
			}
			return next(ctx, retry)
		},
	}
}

func isFireworksGLM52(model damodel.Chat) bool {
	if model == nil {
		return false
	}
	profile := model.Profile()
	return profile.Provider == fireworksGLM52Provider && profile.Model == fireworksGLM52Model
}

func isGLM52TerminalStall(response dagent.ModelResponse) bool {
	if len(response.Structured) != 0 || len(response.Messages) != 1 {
		return false
	}
	message := response.Messages[0]
	if message.Role != damessage.RoleAssistant || len(message.ToolCalls) != 0 {
		return false
	}
	reason, _ := damodel.Outcome(message)
	return reason == damodel.FinishReasonMaxTokens
}

func appendGLM52RecoveryPrompt(system *damessage.Message) *damessage.Message {
	if system == nil {
		value := damessage.System(glm52StallRecoveryPrompt)
		return &value
	}
	value := system.Clone()
	value.Content = append(value.Content, damessage.ContentBlock{
		Type: damessage.BlockText,
		Text: "\n\n" + glm52StallRecoveryPrompt,
	})
	return &value
}
