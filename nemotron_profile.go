package dago

import (
	"context"

	"github.com/semistrict/dago/dagent"
)

var nemotronModelSpecs = []string{
	"NVIDIA:nvidia/nemotron-3-ultra-550b-a55b",
	"nvidia:nvidia/nemotron-3-ultra-550b-a55b",
	"baseten:nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
	"fireworks:accounts/fireworks/models/nemotron-3-ultra-nvfp4",
	"fireworks:accounts/fireworks/models/nemotron-3-ultra-bf16",
	"openrouter:nvidia/nemotron-3-ultra-550b-a55b",
	"nebius:nvidia/Nemotron-3-Ultra-550b-a55b",
	"together:nvidia/nemotron-3-ultra-550b-a55b",
}

const nemotronReadFileDescription = `Reads a file from the filesystem.

Use this tool for text files, source files, documents, images, audio, video, and PDFs.
If the user asks to read, inspect, review, or summarize an entire/whole/full file,
keep reading paginated chunks until you reach EOF or a tool result says the offset
exceeds the file length. A result that contains exactly ` + "`limit`" + ` numbered source
lines is only one page; continue with ` + "`offset + limit`" + ` before giving a final
whole-file answer. Use smaller ` + "`limit`" + ` values for large files to allow automatic
conversation summarization to keep context manageable.

Arguments:
- ` + "`file_path`" + `: absolute path to the file.
- ` + "`offset`" + `: 0-indexed source line to start from; use for pagination.
- ` + "`limit`" + `: maximum source lines to read; use for pagination.

Results are returned with line numbers. Lines longer than 5,000 characters may
be split with continuation markers. Always read a file before editing it.`

const nemotronSystemPrompt = `<approach>
Plan briefly before acting. When several reads or lookups are independent, issue them as parallel tool calls rather than one at a time.
</approach>

<grounding>
Verify state with tools instead of recalling it. Read files before describing
them, use lookup tools for identifiers, and use mutation tools before saying a
requested change is done.
</grounding>

<loop_control>
If a tool call fails, read the error and change the call before retrying; never
re-issue the same failing call unchanged. If a command times out or the same
error repeats, reduce the input, add a termination condition, or switch
approaches before trying again.
</loop_control>

<tool_selection>
Use filesystem tools only for file, path, repository-content, or document
questions. For API, operational, business-object, or other domain questions,
prefer the task-specific non-filesystem tools. For ranking, counting, "which",
or "most" questions over domain entities, enumerate or search candidate
entities with domain tools, fetch the relevant details or counts with matching
domain tools, compare the observed tool results, and answer from that
comparison.
</tool_selection>

<state_changes>
If the user asks to book, cancel, update, send, notify, create, or otherwise
change external state, the change is complete only after the relevant tool call
succeeds. Do not merely describe the intended action or ask the user to assume it
happened. After a successful mutation, use the tool result as the source of truth
for the final answer.
</state_changes>

<final_answer_completeness>
After tool calls succeed, the final answer must include the concrete result, not
just "done". Preserve short exact literals that identify the completed action,
especially versions, titles, and subjects from the user's request or successful
mutation tool arguments/results. If you used an opaque entity ID and an obvious
name or detail lookup tool is available, resolve the ID to human-readable
details before answering. If the user asked multiple questions, answer each one
from its matching tool output; do not substitute an entity from another subtask.
</final_answer_completeness>

<followup_defaults>
Ask follow-up questions only for information needed to proceed safely or
correctly. Do not re-ask for constraints the user already gave. For broad
analysis requests, ask for both the data source and the analysis goal before
using tools. For recurring reports, summaries, monitoring, or support workflows,
treat a stated cadence as sufficient and ask only for missing content, source,
threshold, delivery, or domain details needed to perform the task.
</followup_defaults>

<context_compaction>
If a long conversation switches to a completely unrelated new task and the
compact_conversation tool is available, call compact_conversation before starting
the new task. Also call compact_conversation before reading or summarizing a
large new file after a long conversation.
</context_compaction>`

func init() {
	middleware := nemotronProfileMiddleware()
	for _, name := range nemotronModelSpecs {
		suffix := nemotronSystemPrompt
		if err := RegisterProfile(Profile{
			Name: name, Kind: ProfileHarness, SystemPromptSuffix: &suffix,
			ToolDescriptions: map[string]string{"read_file": nemotronReadFileDescription},
			Middleware:       middleware,
		}); err != nil {
			panic(err)
		}
	}
}

func nemotronProfileMiddleware() []dagent.Middleware {
	return []dagent.Middleware{
		NemotronProgressBudget(NemotronProgressBudgetOptions{}),
		nemotronPolicyNudgeMiddleware(),
		NemotronToolCallShim(),
		NemotronReadContinuationNotice(),
		nemotronFilesystemRetry(),
		NemotronModelRateLimitRetry(),
		nemotronCanonicalMessageCompatibility(),
		NemotronReasoningTagCleanup(),
		NemotronTextToolCallParser(),
		nemotronFollowupDiscipline(),
		nemotronEntityResolutionGuard(),
		nemotronFinalAnswerGuard(),
	}
}

// Canonical messages already preserve tool-call fields and tool-result names,
// so the compatibility layer that upstream needs before provider serialization
// is an explicit no-op at dago's provider-neutral boundary.
func nemotronCanonicalMessageCompatibility() dagent.Middleware {
	return dagent.Middleware{Name: "ChatNVIDIAMessageCompatibilityMiddleware", WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
		return next(ctx, request)
	}}
}
