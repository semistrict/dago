# dago

dago is an idiomatic Go implementation of the Deep Agents SDK and the focused
LangChain/LangGraph behavior it needs. It provides a provider-neutral tool loop,
middleware, required delta channels, durable checkpoints, virtual filesystems,
inline and background subagents, context compaction, skills, memory, and streaming without trying
to reproduce either framework in full.

The public API is currently pre-1.0.

[Try the shelley-in-dago browser demo](https://semistrict.github.io/dago/).

## Install

```sh
go get github.com/semistrict/dago
```

dago requires Go 1.26 or newer.

### Interactive coding agent

`dacode` is a terminal coding agent with durable threads and goals, streaming tool
activity, workspace-aware instructions and skills, and review gates for file writes
and shell commands. It uses Bubble Tea, Bubbles, and Lip Gloss for its terminal
interface.

```sh
go install github.com/semistrict/dago/cmd/dacode@latest
dacode
```

Run the TUI directly without installing it:

```sh
go run github.com/semistrict/dago/cmd/dacode@latest
```

From a dago source checkout, use `go run ./cmd/dacode` instead.

On first run, `dacode` opens OpenAI subscription sign-in and stores its refreshable
session under the user configuration directory. Set `OPENAI_API_KEY` to use API-key
authentication instead; an explicit key takes precedence over the saved session.
Inside the terminal, `/auth` (or `/connect`) opens the credential manager. API-key
input is masked and subscription sign-in can be cancelled without accepting a late
callback. Credential changes apply to newly built model runtimes; they never silently
retarget an in-flight agent.

`dacode config` lists canonical configuration keys, effective values, and sources.
Remote execution is an explicit application capability. Builds that register provider
factories can use `--sandbox PROVIDER`, `--sandbox-id ID`,
`--sandbox-snapshot-name NAME`, and a workspace-contained `--sandbox-setup FILE`.
A bare `--sandbox` uses `sandboxes.default` only after that explicit flag; configuration
alone never creates a remote resource. The stock binary lists the curated provider
contract but fails closed until an authenticated provider factory is linked.
Run `dacode doctor` for bounded offline installation diagnostics, or
`dacode doctor --json` for a stable machine-readable report. Credential values
and provider URL paths, queries, user information, and fragments are omitted.
`dacode install NAME` checks the build's closed optional-integration catalog.
Current releases already ship every listed extra as Go package source, so the command
reports the package or API to import and configure without changing the host. An
application must still import the integration and rebuild; the command does not add it
to the running `dacode` binary. `--package` refuses every name because this build has
no separately loadable package format and never downloads an arbitrary Go module.
`dacode update CHANNEL ARTIFACT --manifest-base URL --public-key /absolute/key.pub`
performs a signed release check; add `--dry-run` to download and verify the artifact
without writing, or `--apply` to replace the current executable. There is deliberately
no built-in release URL or trust key yet: both must be supplied explicitly, manifests
must carry a valid Ed25519 signature, artifacts must match the signed SHA-256 and Go
build identity, and development or locally replaced builds cannot be activated.
See [`docs/UPDATES.md`](docs/UPDATES.md) for the manifest contract and platform notes.
For interactive use, pass the same trust inputs as `--update-channel`,
`--update-artifact`, `--update-manifest-base`, and `--update-public-key`; then bare
`/update` opens the signed-update modal and `/auto-update` controls the persisted
preference. `/notifications` configures dependency warnings, Ctrl+N opens pending
actions, and `/trace` opens the active LangSmith thread only when a stored credential
and an official trusted web endpoint are configured.
`config get KEY`, `config path`, `config set KEY VALUE`, and `config unset KEY`
provide deterministic text or `--json` output and manage the owner-private versioned
configuration file. Runtime precedence is default, config file, canonical environment
variable, `DEEPAGENTS_CODE_` override, `DEEPAGENTS_CLI_` override, then an explicit
command-line flag. An explicitly empty prefixed string shadows the canonical value.
Credentials are never printed or accepted by `config set`; keep them in the
environment or existing sign-in store. Use `--config PATH` or `DACODE_CONFIG` to
select another file.

Normal coding-agent launches also resolve environment files with shell exports first,
the nearest `.env` found from `--cwd` second, and `~/.deepagents/.env` last. Project
files cannot replace process-launch, proxy, TLS, tracing-endpoint, MCP-trust, automatic
review, or terminal-identity controls; ignored key names are reported without their
values. Files, lines, keys, values, ancestor search, and the final environment are
bounded, and symbolic-link environment files are rejected.

Applications can opt into owner-private structured debug traces with
`DEEPAGENTS_CODE_DEBUG=1`. `DEEPAGENTS_CODE_DEBUG_FILE` selects an absolute file
inside an owner-private directory, and `DEEPAGENTS_CODE_LOG_LEVEL` accepts
`DEBUG`, `INFO`, `WARNING`, or `ERROR`. Trace files are bounded and common
credential attributes are redacted, but messages can still contain sensitive
application data and should be reviewed before sharing.

Actions that change files or run commands are routed through a read-only reviewer
that reuses the main model by default. Review failures return to a user decision.
`--approval-model` explicitly selects a separate reviewer, `--manual-review` requires
a user decision for every gated action, and `--yolo` bypasses review. The first
successful Auto-mode
enable shows a versioned safety notice: Enter keeps Auto and records the notice in the
private local state directory, while Esc returns to Manual without recording it. The
first YOLO entry is gated more strictly: YOLO remains inactive until Enter successfully
persists the current warning policy; `m` selects Manual and Esc keeps the prior mode.
The active approval mode is also stored per durable thread under a SHA-256-derived
key. Resuming a thread restores its own mode, and live Shift+Tab or slash-command
changes are persisted before they take effect. Missing, invalid, or unreadable thread
records use Manual. The server consults the same record for gated actions in both the
main agent and delegated subagents, so UI state alone cannot enable unrestricted work.
At a Manual approval prompt, the warning-bordered menu shows numbered Approve,
thread-Auto, and Reject choices for one call or a batch. Arrow keys or `j`/`k` move
the cursor, Enter selects, `1`/`2`/`3` select by position, `y` approves, `a` enables
Auto (or returns an Auto fallback to Manual), `n` or Esc rejects, and Tab opens a
single-line rejection-reason field. Long shell commands can be expanded with `e`;
write, edit, delete, and other tools receive bounded purpose-specific previews, while
recognized credential-file contents are hidden from both the prompt and tool row.
Enter submits a rejection reason (or a bare rejection when blank), while Esc or `n`
first closes the field without deciding. If a gated
action arrives within two seconds of composer input, the decision keys stay attached
to the draft until typing becomes idle; the approval is revealed after at most 30
seconds even if typing continues. The same deferral applies when Auto review falls
back to a user decision.

`/auto model` selects the current session's classifier, Ctrl+S persists the default,
and `/auto model clear` returns to the main model. Auto assesses the complete pending
tool-call batch through a required structured decision tool and requires exact
tool-call ID coverage.
Malformed or unavailable reviews deny conservatively before bounded consecutive,
total-denial, or configuration-fault thresholds return control to the user. A distinct
classifier receives only bounded, redacted trusted-user context and summarized action
metadata, never assistant text or raw tool results.

Use `-n 'task'` for one-shot operation, pipe a task to standard input, or pass
`--stdin` to require piped input explicitly. `--quiet` prints only the final
response, `--no-stream` buffers it, and `--json` emits one versioned result object.
For unattended work, `--max-turns N` and `--timeout SECONDS` bound execution and
exit with status 124 when the bound is reached; the default turn bound is 50.
`--recursion-limit N` separately bounds graph steps within each turn and defaults
to 2000.
Use `-m 'task'` to submit an initial interactive prompt, or `-s NAME` to invoke a
project skill around the initial interactive or one-shot request. Runtime skill
precedence, from lowest to highest, is built-ins, enabled plugin skills, the selected agent profile,
`~/.agents/skills`, project `.deepagents/skills`, project `.agents/skills`,
`~/.claude/skills`, and project `.claude/skills`. `--startup-cmd CMD` runs a local setup
command in the workspace before skill discovery and before the first model turn.
Its literal, bounded output stays local rather than entering model context;
non-zero exits warn and continue, while cancellation stops startup.
Use `dacode skills list|info|create|delete` to manage user or `--project` skills,
and `dacode skills trust list|add|revoke|clear` to manage exact external-symlink
targets. In the TUI, `/skills` lists the effective catalog, `/skill:NAME [task]`
invokes one skill, and `/remember` and `/skill-creator` invoke the standard authoring
skills. An untrusted external symlink opens an exact-target approval prompt; declining
does not read its instructions. The built-in `deepagents-thread-inspector` uses the
read-only `dacode skills inspect-thread` JSON workflow for bounded local summaries,
transcripts, and latest-turn inspection.
Use `dacode plugin list|install|uninstall|enable|disable` and
`dacode plugin marketplace list|add|remove` to manage the owner-private plugin store.
`plugins` is an alias for the same non-interactive command. In the TUI, `/plugins`
opens the Discover, Installed, and Marketplaces views; keyboard actions install,
enable, disable, uninstall, add, or remove entries and then offer a reload. `/reload`
atomically refreshes reloadable environment/configuration values and rebuilds plugin
skills, hooks, MCP connections, and web credentials without discarding the current
runtime if rebuilding fails. Installed plugins are enabled by default and take effect
on the next process, ACP session, or successful reload: skills are
qualified as `plugin@marketplace:skill`, MCP server names are scoped to the plugin,
and declared lifecycle hooks execute as trusted host commands. Plugin installation
therefore grants instruction, process, and network authority; inspect the publisher
and pinned revision before enabling one.
Use `-r ID` to resume a durable thread and `--cwd PATH` to select the workspace.
Run `dacode resume` to choose a session before opening the TUI, or
`dacode resume ID` to resume a known session. Inside the TUI, `/threads` opens
the same picker. Selected sessions restore their transcripts and approval mode before continuing.
Before any transcript is loaded, a session owned by another agent or original
working directory requires an explicit switch/stay decision. Threads above the
configured context threshold also offer an exact-checkpoint compaction choice.
`/offload` (alias `/compact`) runs that same bounded compactor directly and
commits its state only after summary generation succeeds.

The first interactive launch offers a keyboard-driven setup flow and stores its
completion marker and selected preferences in the private state directory.
`/restart` is available when the process owns an explicitly configured local
development server; it opens a confirmation modal and reports restart failures
without exposing the child process or its environment.
`/agents` opens the named-agent picker. Put an agent's instructions in
`<state-dir>/<name>/AGENTS.md`; each agent loads and updates only that durable memory,
and switching starts a new thread. Arrow keys or Tab move through the picker, Enter
switches, Ctrl+S
toggles the startup default, and Esc cancels. The built-in `dacode` agent remains
available and creates an empty memory file on first use. `-a NAME` selects or creates
a profile directly; without it, startup uses `agents.default`, then a valid
`agents.recent`, then the built-in profile. Each profile also has private `skills/`
and `sessions/` namespaces, and session browsing is restricted to the selected agent.
Use `dacode agents list` to inspect profiles and
`dacode agents reset --agent NAME [--target SOURCE] [--dry-run]` to restore the
built-in prompt or copy another profile's prompt. Reset replaces the destination
profile, including its skills and session namespace; preview it before use when that
state matters. Pass
`--memory-auto-save=false`, or set `DEEPAGENTS_CODE_MEMORY_AUTO_SAVE=false`, to load
memory in reference-only mode without asking the model to persist new learnings.
`/docs`, `/changelog`, and `/feedback` open the project documentation, releases,
and issue form in the active browser environment.
Use `/editor` or Ctrl+X to edit the current draft with `$VISUAL`, then `$EDITOR`,
or the platform's default text editor. Saving a blank draft cancels the edit.
Use `/effort` to choose one of the active model profile's reasoning levels,
`/effort LEVEL` to set one directly, or `/effort clear` to restore the provider
default. The selected level is saved per model and shown beside the model name.
Use `/theme` to preview and select the full built-in color catalog. Arrow keys or
Tab preview, Enter saves the global choice, Esc restores the prior theme, `n`
toggles labels and canonical keys, and `t` saves the highlighted theme for the
current `TERM_PROGRAM`. Terminal mappings override the global choice, while
`DEEPAGENTS_CODE_THEME` overrides both. Custom `[themes.NAME]` tables in
`~/.deepagents/config.toml` require a label, may set `dark`, and may override any
semantic color with `#RRGGBB`; `ansi-dark` and `ansi-light` preserve the terminal
palette instead of imposing a background.
The composer completes slash commands and `@` workspace paths, turns valid dropped
images and videos into bounded structured attachments, persists non-command history,
collapses large pastes until submission, and accepts messages while a turn is running
by queueing them. Prefix a draft with `!` to run it in the local shell and
include its bounded output as context for the next request; `!!` runs it locally
without adding the command or output to model context. Shift/Alt/Ctrl+Enter inserts a
newline. Draft action buttons clear or copy the current input.

Local file and `execute` tools use the real host filesystem path shown as the working
directory. Absolute paths are used as-is, relative shell paths start in that directory,
and `/` is the host filesystem root. Remote sandbox sessions instead use the provider's
sandbox working directory and cannot access local host paths.

The transcript renders streamed Markdown, lifecycle-styled tool rows, grouped tool
summaries, skill invocations, and bounded inline write/edit diffs. Ctrl+O expands or
collapses the latest eligible message, tool row, skill, or tool group. `/line-numbers`
toggles gutters for newly created diffs and persists the preference; PageUp hydrates
older rows when a long restored transcript is using its sliding render window.

When the agent needs information it cannot infer, it can pause the active turn with
one or more text or multiple-choice questions. Enter confirms an answer, Tab and
Shift+Tab move between questions, every choice list includes a free-form Other entry,
and Esc dismisses the prompt. Required answers cannot be blank. Ctrl+X edits the
focused free-form answer in the configured external editor. Completed question rows
show only `User answered` or `Question failed`; answer text remains in the durable
model transcript but is excluded from automatic-review audit input.

`--shell-allow-list recommended` makes approved read-only commands execute inline
while rejecting every other shell request instead of pausing for review. Pass a
comma-separated list such as `recommended,git,gh` to add trusted executables.
`--shell-allow-list all` explicitly removes shell-command checks; use it only in an
isolated environment.
One-shot runs expose no shell tool unless a shell allow-list is supplied.

MCP servers are discovered from `~/.deepagents/.mcp.json`, then the project's
`.deepagents/.mcp.json`, then project-root `.mcp.json`; later same-name definitions
win. `--mcp-config PATH` selects one explicit file instead. User and explicit servers
connect at startup. Project servers require a definition-bound remembered approval,
an operator-controlled process allow-list, or the explicit per-run
`--trust-project-mcp` flag. Unapproved or disabled project entries do not connect.
Configured tool names use `SERVER_TOOL`, support allow/deny patterns, and retain MCP
read-only annotations for headless approval policy. OAuth-declared servers remain
disconnected until the separate login flow supplies credentials:

```sh
dacode mcp login <server>
```

The command accepts `--mcp-config PATH` for one explicitly trusted file; otherwise it
uses the same definition-bound project trust gate and precedence as startup. Slack
uses its workspace-aware public-client flow, GitHub Copilot MCP uses its device flow,
and other HTTPS endpoints use MCP dynamic registration with PKCE. Authorization can
open the platform browser or use `--no-browser`; paste-back input remains bounded.
Tokens are stored outside MCP configuration under
`~/.deepagents/.state/mcp-tokens/` with private permissions and are reused only by the
same server name and exact endpoint on a later startup. The login does not mutate a
currently running tool registry.

Within the terminal, `/mcp` opens the current server/tool viewer. Enter expands tools,
starts login, or shows a bounded error; F2 stages a server enable/disable change for
this session. `/mcp login NAME` starts the same endpoint-bound OAuth policies and
`/mcp reconnect` rebuilds the idle runtime. Login and enablement changes remain pending
until reconnect succeeds; Esc defers them without discarding the pending state.

Use `/goal <objective>` for work that should continue across turns until complete or
blocked. The agent drafts observable acceptance criteria first; accept, edit, reject
with feedback, or cancel that proposal in the review panel. Accepted criteria remain
visible with the durable goal and are evaluated before a requested completion is
committed. `/goal show`, `/goal amend <feedback>`, `/goal pause`, `/goal resume`,
`/goal clear`, and `/goal budget <tokens|clear>` control the persisted goal.
`/goal model` and `/goal max-iterations` are aliases for its rubric grader settings.
Active goals resume automatically when the thread becomes idle. Pass `--goal TEXT`
to open the same review flow at interactive startup.

Use `/workflows` to open the live workflow control panel. It shows background run
status, current phase, agent progress, and errors; use the arrow keys to select a run
and `c` to cancel it. `/workflow <saved-name-or-script-path>` launches a saved
workflow directly. Names resolve from `.claude/workflows` and `.agents/workflows` in
the workspace, while explicit paths are restricted to the workspace and application
state directories.

Use `/rubric set <criteria>` for a persistent quality gate, `/rubric next <criteria>`
for one turn, or `/rubric file <path>` to load a UTF-8 criteria file. `/criteria` is
an alias. `/rubric show`, `/rubric clear`, `/rubric model`, and
`/rubric max-iterations` inspect and tune grading. For one-shot execution, combine
`-n TASK` with `--rubric 'criteria'` or `--rubric @path`; optional
`--rubric-model` and `--rubric-max-iterations` select the grader behavior.

Use `dacode acp` (or `--acp`) to serve the coding agent to an ACP-compatible editor
over standard input and output. The editor owns the session and permission prompts
in this mode; `--yolo` remains available to bypass mutating-tool approval gates.
Each editor session gets its own workspace runner and any declared stdio, HTTP, or
SSE MCP servers. HTTP headers are forwarded for per-session MCP authentication.

`--serve-xtermjs` serves the same PTY-backed TUI on a loopback-only web address
and prints its URL. Use `--xtermjs-address HOST:PORT` to select a specific
loopback listener.

For an explicitly managed local development companion, pass an absolute
executable with `--local-dev-server PATH`. Repeat `--local-dev-arg VALUE` for
literal arguments and select its loopback readiness origin/path with
`--local-dev-endpoint` and `--local-dev-health-path`. The child receives only a
typed, non-credential configuration payload and a small platform environment;
additional non-secret names require an explicit `--local-dev-inherit-env NAME`.
The process tree is stopped when either interactive or one-shot execution exits.
This opt-in supervisor is unavailable in ACP server mode.

Use the repository-root GitHub Action for bounded CI tasks, durable cache-backed
agent memory, and optional skills-repository installation. See [`ACTION.md`](ACTION.md)
for its inputs, secure defaults, and a pinned workflow example.

### Managed-agent project scaffold

`dago init [name]` creates the pinned managed-agent starter layout: `agent.json`,
`AGENTS.md`, an empty `tools.json`, `.gitignore`, an example skill, and a researcher
subagent. When the name is omitted, the command prompts for it. Existing directories
are left untouched unless `--force` is explicit; force refreshes only the starter
files and preserves unrelated content.

Bare `dago` no longer starts an interactive chat. Run `dacode` for the interactive
coding agent; `dago` is reserved for explicit project and managed-service commands.

```sh
dago init my-agent
cd my-agent
```

Managed workspaces can also list, inspect, and explicitly delete remote agents.
Authentication resolves `LANGSMITH_API_KEY` before `LANGCHAIN_API_KEY`; the endpoint
resolves the corresponding `LANGSMITH_ENDPOINT` / `LANGCHAIN_ENDPOINT` variables
and otherwise uses the hosted API. Only HTTPS origins are accepted.

```sh
dago agents list
dago agents get --include-files AGENT_ID
dago agents delete AGENT_ID
```

Deletion prompts unless `--yes` is supplied.

Deploy the current scaffold with `dago deploy`. The first run creates a remote
agent; later runs patch metadata and synchronize only the managed `AGENTS.md`,
`tools.json`, `skills/`, and `subagents/` directory projection. The remote ID is
pinned outside the checkout under `~/.deepagents/deployments/`, keyed by the
project’s absolute path and authenticated endpoint.

Creation uses a stable server idempotency key and recovery marker derived from the
authenticated endpoint and required project-owned `extras.dago_deployment_id`.
`dago init` generates that identifier and preserves it during forced scaffold
refreshes. To make `--reset` create a genuinely new remote agent, rotate the identifier
in `agent.json` first; this keeps the new logical creation explicit and portable.

```sh
dago deploy --dir ./my-agent --dry-run
dago deploy --dir ./my-agent
```

`--detach` skips the health request, `--reset` forgets cached state and creates a
new agent, and `--yes` acknowledges an `agent_id` explicitly declared by
`agent.json`. A declared remote must already carry this project's deployment
identity; the client will not race to adopt an unbound agent. Dry runs require no
credential and perform no state or network I/O.

The same authenticated workspace can manage its MCP server registry. Identifiers
accept an exact ID, unique name, or normalized URL. Static headers are supplied
explicitly and redacted from inspection output; OAuth records can be registered
without storing a token in the project.

```sh
dago mcp-servers list
dago mcp-servers add --name tools --header X-Api-Key=VALUE https://tools.example
dago mcp-servers add --auth-type oauth --connect --no-browser https://oauth-tools.example
dago mcp-servers get tools
dago mcp-servers update tools --url https://new-tools.example
dago mcp-servers connect --no-browser tools
dago mcp-servers tools tools
dago mcp-servers delete tools
```

Tool discovery prints a paste-ready `tools.json` snippet. OAuth connection registers
the workspace provider, prints its HTTPS verification URL, optionally opens the
platform browser, and performs bounded long polling; use repeated `--scope`,
`--force-new`, `--timeout`, or `--no-browser` as needed. Deletion prompts unless
`--yes` is explicit.

## Use dago as a library

Models implement the small `damodel.Chat` interface, and tools implement
`datool.Tool`. This complete example uses the OpenAI adapter and a typed local tool;
the agent and tool APIs remain provider-neutral:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/datool"
)

type addInput struct {
	A int `json:"a" description:"First number"`
	B int `json:"b" description:"Second number"`
}

func main() {
	chat := openai.NewAPIKey(os.Getenv("OPENAI_API_KEY"), "gpt-5", openai.Options{
		ContextWindow: 128_000,
	})
	add := datool.New("add", "Add two integers.", func(_ context.Context, input addInput) (int, error) {
		return input.A + input.B, nil
	})

	agent := dago.New(chat,
		dago.WithSystemPrompt("Use tools when they help answer accurately."),
		dago.WithTools(add),
	)
	result, err := agent.Invoke(context.Background(), dagent.Prompt("What is 17 plus 25?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result)
}
```

Filesystem tools are opt-in through `dago.WithFilesystem`. With the default state
backend, files live in the agent’s `files` delta channel, are isolated by thread,
and become durable when a checkpoint saver is configured. Pass an explicit store,
composite, local-shell, or remote sandbox backend when the agent should operate
elsewhere.

Agent-owned facilities use that same backend automatically. Configure them as values
instead of constructing middleware with a duplicate backend argument:

```go
compiled := dago.New(chat,
	dago.WithBackend(workspace),
	dago.WithSkills(dago.Skills{Sources: []string{"/skills"}}),
	dago.WithMemory(dago.Memory{Sources: []string{"/AGENTS.md"}}),
)
```

Memory sources use the backend's virtual paths. Machine-managed onboarding blocks
inside those files are protected automatically, including for subagents that share
the filesystem. Set `Memory.ReadOnly` to use the built-in reference-only prompt while
still loading the configured files.

Durable goals are an opt-in agent facility and require a checkpoint saver. The
middleware exposes `create_goal`, `get_goal`, `get_rubric`, and constrained
`update_goal` tools; `dagoal.Service` provides host-owned pause, resume, budget,
objective, criteria, and clear operations. When accepted criteria must gate goal
completion, place `dagoal.RubricCompletionMiddleware` before `dago.Rubric` so a
model-requested completion is committed only after a satisfied verdict:

```go
goalOptions := dagoal.Options{}
compiled := dago.New(chat,
	dago.WithMiddleware(
		dagoal.Middleware(goalOptions),
		dagoal.RubricCompletionMiddleware(dago.RubricStatusKey, string(dago.RubricSatisfied)),
		dago.Rubric(chat, dago.RubricOptions{}),
	),
	dago.WithSaver(saver),
)
goals := dagoal.NewService(compiled, goalOptions)
```

Binary media is returned opaquely by default. Supplying
`dago.WithFilesystem` can configure a `VideoExtractor`, which changes video
`read_file` pagination to seconds and returns sampled JPEG frames. `davideo.NewFFmpeg` is the optional
ready-made implementation; the FFmpeg executable remains an external deployment
dependency.

Declarative subagents use the same functional options as top-level agents. A nil
model inherits the parent model, and tools inherit unless `WithTools` overrides them:

```go
researcher := dago.NewSubagent(
	"researcher",
	"Researches a topic and returns a concise answer.",
	nil,
	dago.WithSystemPrompt("Research the delegated topic."),
	dago.WithTodo(),
)
compiled := dago.New(chat, dago.WithSubagents(researcher))
```

They always receive tool-call repair and inherit parent facilities that were
explicitly enabled for declarative children, including filesystem, interpreter,
memory, summarization, approval, and prompt caching. Precompiled graphs can be registered with `NewRunnableSubagent`; only
delegation options such as `WithInheritedState` apply because their construction is
already complete. Human approval, including approval inside a subagent, requires a
checkpoint saver so the exact pending tool call can resume without replaying completed
sibling tools.

`dacode` also discovers declarative subagents from `NAME/AGENTS.md` directories.
Project definitions under `.deepagents/agents` override same-name definitions in the
selected agent profile's `agents` directory. Frontmatter requires `description`, may
set `name` and `model`, and the remaining Markdown is the subagent system prompt. An
omitted or empty model inherits the main model. Defining `general-purpose` replaces
the built-in general agent; otherwise the built-in remains available.

### Workflows

Workflows are an optional extension that lets a model launch a deterministic
JavaScript orchestration script in the background. The host supplies a
`daworkflow.AgentRunner`, which is the policy boundary for models, reasoning
effort, custom agent types, structured output, and worktree isolation. The manager
owns cancellation and same-session replay and should be closed with its application
scope:

```go
manager := daworkflow.NewManager(runner, daworkflow.Options{
	SessionDirectory: sessionDir,
})
defer manager.Close()

compiled := dago.New(chat,
	dago.WithMiddleware(daworkflow.Middleware(manager)),
)
```

The `workflow` tool accepts an inline `script`, a resolver-backed saved `name` or
`script_path`, or a completed `resume_from_run_id`, and immediately returns task and
run IDs. Scripts begin with a runtime-evaluated `export const meta = {...}` declaration
and can use `agent`, `parallel`, `pipeline`, `phase`, `log`, `args`, `budget`, and one
level of nested `workflow` calls. The runtime has no filesystem or Node APIs and
rejects implicit clocks and randomness. Concurrency, agent count, collection size,
memory, execution time, and tokens are bounded through `daworkflow.Options`. When
`SessionDirectory` is set, the manager persists the script, per-agent transcripts,
replay journal, and final JSON result under that directory. `Options.Completed` can
bridge terminal runs into the host application's native task-notification channel.

The terminal host implements `isolation: "worktree"` for local Git workspaces. Each
isolated agent call starts from the configured workspace's committed `HEAD` on a new
`workflow/agent-*` branch. A clean checkout is removed after the call; an unchanged
branch is removed, a committed branch is retained, and a checkout with uncommitted
changes is retained and reported as an agent failure with its recovery path. Worktree
isolation is a coordination boundary rather than a security sandbox, and it is not
available when the terminal host is using a remote or custom backend.

### Type-safe tools

`datool.New` derives an object schema from the handler's input struct, validates
model arguments, and decodes them before calling the handler:

```go
type weatherInput struct {
	Location string `json:"location" description:"City and state" jsonschema:"minLength=1"`
	Units    string `json:"units,omitempty" jsonschema:"enum=celsius|fahrenheit,default=celsius"`
}

weather, err := datool.New("weather", "Get the current weather.", func(ctx context.Context, input weatherInput) (string, error) {
	return input.Location + ": sunny", nil
})
```

Fields use `encoding/json` names and `omitempty` behavior. A `description` tag
sets field documentation; `jsonschema` supports requirements, formats, enums,
defaults, examples, string/array/object lengths, patterns, and numeric bounds.
Handlers that need call state can use `datool.RuntimeFromContext(ctx)`.
Returning a `datool.Result` preserves its full content, artifact, state update,
interrupt, and handoff; strings become text results and other values become JSON text.
Runtime schema details can be layered onto the generated schema with functional
options such as `WithPropertyType`, `WithPropertyEnum`, `WithPropertyValue`,
`WithPropertySchema`, `WithoutProperty`, or the lower-level `WithTransformSchema`.

### Web tools

The opt-in `daweb` package provides `http_request` and HTML-to-Markdown `fetch_url`
tools. Supplying a Tavily key also enables `web_search`; a blank key leaves that tool
out of the returned set:

```go
webClient := daweb.NewClient(daweb.Options{})
tools := daweb.Tools(webClient, os.Getenv("TAVILY_API_KEY"))
compiled := dago.New(chat, dago.WithTools(tools...))
```

The client accepts only HTTP and HTTPS URLs, rejects credentials and non-public
address ranges, pins each connection to its validated DNS answers, revalidates every
redirect, ignores environment proxies, and bounds request, response, rendered page,
redirect, and timeout resources. Library applications opt in explicitly. The coding
agent includes `fetch_url` by default and prefers a model provider's hosted web-search
tool, including OpenAI and Anthropic integrations. OpenAI search is enabled by default;
`--model-params '{"web_search":false}'` disables it. When the resolved model does not
provide web search, `dacode auth set tavily` or `TAVILY_API_KEY` supplies an
approval-gated local fallback. Stored service credentials take precedence over
environment values. If both are configured, provider-hosted search wins and the local
`web_search` tool is not exposed.

Typed adapters keep the state and checkpoint wire formats flexible without
requiring application assertions. Use `dagent.Field` with a
`dagent.FieldSpec[T]` to declare typed reducers, `datool.StateAs[T]` for tool
state, and `dagent.DepsAs[T]` or `datool.DepsAs[T]` for application dependencies
supplied through `WithDependencies`. `dagent.ResumeAs[T]` accepts both live Go values
and checkpoint-restored plain JSON values. Structured results can be declared
with `dagent.StructuredOutputFor[T]` and decoded with
`dagent.StructuredAs[T]`; the latter validates against the schema derived from T.
`damessage.MetadataAs[T]` and `damessage.SetMetadata` provide the same typed
boundary for raw JSON metadata maps.

`dagent.RuntimeModel` consumes the `model` configurable value, resolves it through
a required caller-owned `ModelResolver`, swaps only that invocation's model, and
persists the selected spec in private thread state unless `Ephemeral` is requested.
The resolver retains ownership of provider clients, credentials, caching, and any
network access; the middleware bounds the model spec and contains resolver panics.

External local event ingress is opt-in through `daeventbus`. Supply the sink and
an absolute Unix socket path positionally, then run the source under an owning
context:

```go
source := daeventbus.NewUnixSource(sink, socketPath, daeventbus.Options{})
err := source.Run(ctx)
```

Each newline-delimited JSON object carries `kind` (`command`, `prompt`, or
`signal`), `payload`, and optional `bypass` and `correlation_id` fields. The
source replies once per line with an ACK or bounded NACK. It only validates and
forwards events: the application-owned sink decides what any command, prompt,
signal, or bypass hint means. Unix ingress reports an explicit unsupported error
on Windows rather than silently opening another transport.

Owned agent streams support `for event, err := range stream.Events()`. Model
streams support `for chunk, err := range stream.Chunks()`. Both iterators close
their stream on completion, error, or early loop exit; `Next` and `Close` remain
available for explicit control.
Applicable built-in Anthropic harness profiles resolve from the model's provider and
identifier. The full Nemotron 3 Ultra repair, retry, progress-budget, tool-selection,
entity-resolution, and answer-completeness stack is available explicitly from
`daproviders/nemotron`. Provider construction defaults for OpenAI, NVIDIA, and
OpenRouter are available as an explicit `daproviders/profile.Profiles` value.
`daproviders/profile.Resolver` composes those profiles with caller-owned provider
factories to resolve `provider:model` strings without hiding credential or optional
dependency ownership. Model-spec matching normalizes known provider aliases, and
Bedrock detection covers provider-prefixed and Amazon Nova identifiers.

Full provider selection is available from `daproviders/modelconfig`. It combines the
pinned provider catalog, stored/environment credential precedence, paired base URLs,
model parameters, profile overrides, retries, and owner-private default/recent
preferences while keeping provider factories explicit. See [model
configuration](docs/MODELS.md). `dacode` exposes `--model-params`,
`--profile-override`, `--max-retries`, `--default-model`, and
`--clear-default-model`; selecting an integration not compiled into the application
fails explicitly without network or package discovery.

`--model claude_agent:sonnet` selects the compiled Claude CLI provider. It uses
print-mode bidirectional stream JSON, disables Claude's built-in tool and local
customization surfaces, and exposes only the current agent request's tools through an
ephemeral authenticated loopback MCP server. The outer dago agent executes those
tools and returns their results through MCP while the same CLI process remains alive;
partial text and reasoning events stream through `dacode` as they arrive. Native JSONL
reconstruction and `--resume` are used only after a process restart. See [model
configuration](docs/MODELS.md#coding-agent-flags) for the isolation boundary and
supported parameters.

`--model anthropic:MODEL` selects the direct Messages API provider. Hosted web search
is enabled by default, and `hosted_tools`, `mcp_servers`, `betas`, plus forward-compatible
top-level Messages parameters can be supplied with `--model-params`.

Local Ollama model discovery is explicit and credential-free. Supply the HTTP
transport and endpoint positionally, then call `Discover` when a local model picker
or configuration flow needs a refresh:

```go
discovery := ollama.NewDiscovery(http.DefaultTransport, "", ollama.DiscoveryOptions{})
models, err := discovery.Discover(ctx)
```

An empty endpoint uses `http://127.0.0.1:11434`. Discovery accepts only literal
loopback HTTP(S) origins (the exact `localhost` name is rewritten to a literal),
performs one bounded `/api/tags` request, sorts and deduplicates names, and never
adds authentication. It is not invoked automatically by the agent or provider
resolver.

LangSmith LLM Gateway routing is similarly explicit and provider-neutral. Supply
a caller-owned model factory and gateway key positionally; an empty endpoint uses
the managed gateway:

```go
gateway := langsmithgateway.NewResolver(factory, "", gatewayKey, langsmithgateway.Options{})
model, err := gateway.ResolveModel(ctx, "openai:gpt-4.1")
```

The factory receives the routed endpoint, key, and original model specification
and remains responsible for provider SDK construction and network policy. The
built-in pinned routes cover Anthropic, Baseten, Fireworks, Google GenAI, and
OpenAI. Construction and endpoint inspection perform no I/O.

## JavaScript interpreter

Enable a persistent, sandboxed QuickJS-ng REPL with `Interpreter`. The `js_eval`
tool supports top-level await, console output, functions and variables that persist
through checkpoints, and concurrent programmatic tool calls. It runs the exact
`quickjs-rs` 0.2.5 WASM guest under Wazero's interpreter backend, including in Go
browser-WASM builds. TinyGo builds exclude the Wazero-backed implementation;
enabling `Interpreter` in a TinyGo build fails during agent construction, and the
Shelley TinyGo application omits `js_eval` from its tool catalog.

```go
compiled := dago.New(chat,
	dago.WithSaver(saver),
	dago.WithInterpreter(dago.Interpreter{
		PTC: []string{"read_file", "glob", "grep", "search"},
	}),
)
```

The first memory image is checkpointed as an anchor. The generated QuickJS guest
uses WAFL-style 4 KiB write barriers, so subsequent checkpoints copy only dirty
memory pages. A nil `PTC` allowlist exposes only `read_file`, `glob`, and
`grep`; an empty non-nil list disables PTC. Explicitly allowlisted tools execute
inside `js_eval` and do not pass through model-tool approval middleware, so mutating
tools should only be included when that direct authority is intended.

## OpenAI adapter

The focused Responses API adapter supports text and multimodal messages, tool calls,
parallel tool calls, JSON Schema structured output, token streaming, usage, prompt
caching metadata, API keys, and an explicit subscription OAuth session. Standard
OpenAI endpoints use persistent Responses WebSocket connections by default and send
incremental input on compatible successive turns. Set `ResponsesWebSocket` to
`new(false)` to force HTTP; compatible custom endpoints can opt in with
`new(true)`. Standard endpoints also enable remote server-side compaction by default:
at 90% of `ContextWindow` (or 200,000 tokens when it is unknown), the adapter sends a
compaction-trigger Responses request, preserves its encrypted state, and resumes the
turn. Set `ServerCompaction` to `new(false)` to disable it or set
`CompactionThreshold` to override the trigger point.

```go
chat := openai.NewAPIKey(os.Getenv("OPENAI_API_KEY"), "gpt-5", openai.Options{
	ContextWindow: 128_000,
})
```

The core package never discovers credentials or chooses a provider. OAuth token
persistence is opt-in and writes only to the caller-provided private file.

## OpenRouter adapter

The OpenRouter adapter uses OpenRouter's Responses API and preserves the same
text, multimodal, tool-calling, structured-output, reasoning, web-search, usage,
and streaming contracts as the OpenAI adapter. It also supports optional app
attribution and provider routing:

```go
chat := openrouter.New(os.Getenv("OPENROUTER_API_KEY"), "anthropic/claude-sonnet-4.6", openrouter.Options{
	AppURL:   "https://example.com/my-agent",
	AppTitle: "My Agent",
	Routing: &openrouter.ProviderRouting{
		Ignore:         []string{"azure"},
		DataCollection: "deny",
	},
	ContextWindow: 200_000,
})
```

`BaseURL` defaults to `https://openrouter.ai/api/v1`. Credentials remain
explicit; the adapter does not read environment variables itself.

## Durable execution

```go
saver, err := sqlite.Open("agent-checkpoints.sqlite")
if err != nil {
	log.Fatal(err)
}
defer saver.Close()

compiled := dago.New(chat, dago.WithSaver(saver))
result, err := compiled.Invoke(ctx,
	dagent.FromCheckpoint(dacheckpoint.Config{ThreadID: "conversation-1"}),
	dagent.Prompt("Inspect the project."),
)
```

Agents expose checkpoint history, replay, thread fork, and thread deletion. SQLite
and PostgreSQL savers match the supported Python table layouts and delta-snapshot rules.
Cross-language payload compatibility is intentionally limited to the safe plain-data
subset in [`docs/SERIALIZATION.md`](docs/SERIALIZATION.md); Python-specific object
records are rejected with typed context instead of reconstructed.

## Long-running agent host

`datalon` is an experimental, provider-neutral host for local assistants that stay
running across channel messages and scheduled jobs. It owns one runtime, any number
of channel adapters, and an optional scheduler; starts and stops them in dependency
order; serializes work per channel conversation; and lets `/stop` cancel the active
turn and discard turns already queued behind it. Different conversations can still
make progress concurrently.

The zero-value configuration uses the current working directory, a 500-step
recursion limit, finite message/send/shutdown bounds, and private assistant state at
`~/.deepagents/default/`. Set a stable assistant ID to isolate another state tree.
Passing a nil runtime deliberately selects the echo runtime, which makes channel and
scheduler integration testable before a model-backed runtime is configured.

Channel tool approval is an experimental convenience for a trusted operator, not a
production authorization boundary. `datalon/approval.FromEnvironment` parses the
comma-separated `DEEPAGENTS_TALON_INTERRUPT_ON_TOOLS` overlay with finite limits;
listed local or MCP tool names are exact and always override a same-name disabled
base rule. Apply the resulting rules to the main agent and inherited subagents, then
resolve every `human_approval` interrupt through the invocation's channel handler:

```go
policy, err := approval.FromEnvironment(nil, approval.Options{})
if err != nil {
	return err
}
rules := policy.ApprovalRules(applicationRules...)
agent := dago.New(chat, dago.WithApprovalRules(rules...))

// In the runtime's bounded invoke/resume loop:
resume, err := approval.ResolveInterrupt(ctx, request, result.Interrupts[0])
```

`WithApprovalRules` carries the combined rules into dago's built-in general-purpose
and declarative subagents. A separately compiled or remote subagent is a separate
execution boundary and must receive the same policy itself. Handlerless scheduled
runs, timeouts, cancellation, transport errors, malformed requests, invalid replies,
and unsupported decisions all fail closed. Only a recognized reply from the exact
origin channel conversation and initiating sender resolves one pending approval;
spoofed, stale, duplicate, or ordinary messages remain on the serialized agent path.

Fleet exports can be materialized into an explicit assistant state directory before
starting the host:

```sh
go run ./cmd/datalon import-fleet <fleet-export.zip> <assistant-state-dir>
```

The importer writes `AGENTS.md`, `skills/`, and remapped `agents/<name>/AGENTS.md`
prompts. Fleet `tools.json` files are import input only, and `config.json` is ignored.
Requested remote tools produce a credential-free OAuth `.mcp.json` plus a
human-readable `.mcp.json.setup` handoff. The completion summary recommends a
`DEEPAGENTS_TALON_INTERRUPT_ON_TOOLS` value when the export marked tools for approval.
Repeated imports refresh only these importer-managed paths and preserve unrelated
assistant runtime state.

`datalon/mcp` loads the resulting MCP configuration without reading credentials
from it. Resolution checks `DEEPAGENTS_TALON_MCP_CONFIG`, then `MCP_CONFIG`, then
`~/.deepagents/.mcp.json`. HTTP, SSE, and trusted local stdio servers are supported,
including allow/deny tool patterns and per-server load status. Applications supply
their HTTP policy and may connect the discovered tools to any model-backed runtime:

```go
oauth := mcp.NewOAuthFactory(httpClient, tokenStore, interaction)
loader := mcp.NewClient(httpClient, oauth, mcp.Options{})
tools, configPath, err := loader.LoadDiscovered(ctx, nil, "")
defer tools.Close()
```

Applications that need the pinned provider policies can instead use
`oauthpolicy.NewFactory`: Slack uses its public client with PKCE, state validation,
and optional workspace selection; GitHub Copilot MCP uses its public device flow;
all other HTTPS servers retain standards-based metadata discovery and dynamic client
registration. The required HTTP client, token store, and UI-agnostic interaction are
still supplied by the application.

OAuth tokens stay in a caller-selected private token store. The command-line flow
uses `~/.deepagents/.state/mcp-oauth/` and paste-back authorization, so it does not
need to open a browser or own a listener:

```sh
go run ./cmd/datalon mcp config
go run ./cmd/datalon mcp login <server>
```

`datalon/cron` adds a versioned `cron/jobs.json` store, a minute-granularity
persistent scheduler, and conversation-scoped `create_job`, `list_jobs`, `edit_job`,
and `remove_job` tools. One-shot and recurring jobs are claimed on disk before they
run, so a restart cannot repeat the same interval. Each run persists its last status
or bounded error and emits `talon_event` JSON lifecycle records; results beginning
with `[SILENT]` deliberately skip delivery.

`datalon/lifecycle` applies bounded retention to sensitive assistant state.
Its zero options preserve completed cron records for 30 days, remove inbound
media after 24 hours, and enforce the pinned 1 GiB global artifact ceiling.
State root and cron store are required positionally; construction performs no
I/O. Operators can preview the same plan without deletion:

```go
retention := lifecycle.New(config.StateDir(), cronStore, lifecycle.Options{})
preview, err := retention.DryRun(ctx)
report, err := retention.Clean(ctx)
```

Reports expose only counts and hashed audit references, never prompts, paths,
filenames, content, or job IDs. The entire finite walk is validated before file
deletion, linked and special state fails closed, cron replacement remains atomic,
and cleanup secures managed state owner-only. Durable channel sessions and remote
traces are preserved unless the caller explicitly opts a confined local artifact
directory into retention; session deletion additionally requires a static risk
acknowledgement. See [`datalon/lifecycle`](datalon/lifecycle/README.md).

Per-run LangSmith tracing is opt-in through `datalon/tracing`. The provider-neutral
wrapper preserves the runtime result even when tracing fails, bounds copied inputs,
outputs, errors, and metadata, and records channel or scheduler metadata on one root
run per invocation. The environment helper follows the Talon opt-in contract:
`LANGSMITH_TRACING` must be truthy and `LANGSMITH_API_KEY` must be non-empty.

Long-running applications can use `tracing.NewManager` for the broader coding-agent
policy. A required credential store is resolved once against a caller-supplied
environment snapshot; a stored key enables tracing unless a recognized flag opts
out, agent traces receive their own project, the original tracing variables can be
restored for shell subprocesses, and orphaned tracing fails closed. Region aliases,
bounded replica projects, and replica ingestion endpoints are supported without
mutating process-global environment. `Configuration.ResolveSink` passes credentials
only to a required caller-owned provider factory, and `tracing.NewManaged` redacts
known credential values while emitting the primary and replica spans.

```go
langsmithClient := langsmith.NewClient()
defer langsmithClient.Close()
runtime = tracing.NewFromEnv(
	runtime,
	langsmithtrace.New(langsmithClient),
	config.AssistantID,
	nil,
	tracing.Options{},
)
```

The zero options use project `deepagents-talon`, run name `talon.agent`, and finite
payload and completion bounds. The application owns endpoint policy, credentials,
client shutdown, and flushing.

`tracing.NewURLResolver` separately resolves a project web URL through a required
caller lookup, caches only validated HTTPS results with finite timeout/TTL/count
bounds, and builds credential-free thread links. It never opens a browser; terminal
or headless presentation remains an application concern.

Inbound voice transcription is opt-in through `datalon/speech`. Wrap any channel
that supplies a confined local `voice_path` or `media_path`; voice and video messages
receive the transcript while ordinary files pass through unchanged. `NewLocal`
defaults to `nvidia/parakeet-tdt-0.6b-v3` on CPU, converts a private bounded copy to
16 kHz mono WAV with ffmpeg, and invokes the optional local Transformers pipeline
through `python3`. Set the device to `cuda` for compatible local hardware. For a
non-Parakeet model, `NewOpenAI` requires the caller's HTTP client, API key, and model
positionally and uses the bounded audio-transcription endpoint. The environment
parser also accepts legacy `SPEECH_ENABLED` and `SPEECH_DEVICE` values.

Telegram is an additive Bot API channel. Its token and HTTP client are required
positional dependencies; construction performs no network work. Long polling is the
default, uses finite request/retry/payload limits, and can persist its update offset:

```go
telegramChannel := telegram.New(
	os.Getenv("TELEGRAM_BOT_TOKEN"),
	http.DefaultClient,
	telegram.Options{
		Exposure: telegram.AllowlistExposure(
			[]string{"123456789"},
			[]string{"-1001234567890"},
		),
	},
)
host := datalon.NewHost(runtime, config, telegramChannel)
```

Private messages are selected by user ID, channel posts by chat ID, and group or
supergroup traffic is ignored. `telegram.SelfExposure` requires operator IDs;
`telegram.OpenExposure` requires the exported risk acknowledgement. The alternate
`telegram.NewWebhook` constructor requires its webhook secret positionally and returns
an `http.Handler`, while the application continues to own TLS, routing, listener
lifecycle, source-address policy, and webhook registration.

WhatsApp is an additive channel backed by the packaged Node bridge. The bridge
binds only to loopback, requires bearer authentication, persists pairing state
in an owner-private directory, and reports `qr_pending` while its operator QR is
shown on standard output. The application supplies an authenticated transport
and stable session directory positionally and owns the Node process lifecycle:

```go
whatsappTransport := whatsapp.NewHTTPTransport(
	"http://127.0.0.1:3000",
	os.Getenv("WHATSAPP_BRIDGE_TOKEN"),
	whatsapp.HTTPOptions{},
)
whatsappChannel := whatsapp.New(
	whatsappTransport,
	"/private/operator/whatsapp-session",
	whatsapp.Options{},
)
```

Self-only exposure is the default; allowlists accept exact conversations or
case-sensitive mention patterns, and open exposure requires an explicit risk
acknowledgement. Outbound chunks carry a bot header. Media is hard-capped at
64 MiB and staged only from a caller-confined outbound root. See
[`datalon/whatsapp`](datalon/whatsapp/README.md) for bridge setup and security
boundaries.

## Agent Client Protocol

`daacp` exposes an agent to ACP-compatible editors over newline-delimited JSON-RPC.
The process must reserve standard output for protocol messages; send logs to standard
error.

```go
server := daacp.New(compiled, daacp.Options{
	Name:    "workspace-agent",
	Version: "1.0.0",
})
if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
	log.Fatal(err)
}
```

The adapter supports ACP v1 session creation and durable loading with replay-marked
human, assistant, and tool history. A load is accepted only when its absolute working
directory matches the directory stored with the original session. Factory-backed
servers created with `daacp.NewFactory` can advertise model choices: changing the
`model` Session Config Option rebuilds that session's runner with the selected model
in `daacp.AgentSessionContext` while retaining its checkpointed conversation. With
durable loading enabled, the replacement runner also persists the selection through
`daacp.SessionConfigSaver`, and `session/load` restores it before replaying history.
Only advertised model identifiers reach the factory. `dacode acp` advertises the
startup model first, followed by its supported OpenAI model choices.

The adapter also supports
prompts, cancellation, close, authentication handshakes, session
configuration negotiation, streamed text and reasoning, tool status and progress,
plans, and approve/reject permission requests. A session factory can construct an
isolated runner from the requested working directory and MCP declarations; stdio,
HTTP, and SSE MCP transports are supported by `dacode`. The session working
directory is also available to tools as `daacp.ConfigurableCWD`. Additional roots,
client filesystem/terminal delegation, and ACP-routed MCP transport are not
advertised.

## LangSmith Studio

`dago dev` exposes configured Go agent factories through the LangGraph Agent
Server protocol, persists development threads and store values in SQLite, watches
Go/module/config/environment files, and rebuilds the server when they change.

```sh
go install github.com/semistrict/dago/cmd/dago@latest
```

Export a factory that accepts the server-owned runtime. Using its saver and store
is required for Studio state, history, replay, and thread operations to address the
same durable data as agent runs:

```go
func NewAgent(_ context.Context, runtime daserver.Runtime) (*dagent.Agent, error) {
	return dago.New(chat,
		dago.WithSaver(runtime.Saver),
		dago.WithStore(runtime.Store),
		dago.WithDependencies(runtime.Deps),
	), nil
}
```

Point `dago.json` at the package and exported factory:

```json
{
  "graphs": {
    "agent": {
      "path": "./agent:NewAgent",
      "description": "Workspace agent"
    }
  },
  "env": ".env"
}
```

Then start the API and Studio:

```sh
dago dev
```

The default API is `http://localhost:2024`; `--no-browser`, `--host`, `--port`,
`--config`, and `--n-jobs-per-worker` are available. Development data and the
generated wrapper stay under the ignored `.dago_api` directory. The
[`examples/studio`](examples/studio) configuration is a network-free smoke test:

```sh
dago dev -c examples/studio/dago.json
```

This is a focused local Studio integration, not a claim that arbitrary LangGraph
applications can run in Go. Its supported Agent Server resources and current
limits are listed in [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md).

## Packages

| Package | Purpose |
|---|---|
| `dago` | Deep Agent constructor plus filesystem, JavaScript interpreter, subagent, summary, skill, memory, profile, and rubric middleware |
| `dagent` | Provider-neutral model/tool graph, middleware lifecycle, approval, retry, todo, streaming, and checkpoint operations |
| `dagoal` | Durable goal state, model tools, host lifecycle controls, accounting, and continuation messages |
| `dacost` | Bounded streaming token accounting, provider/model/purpose reports, and local pricing catalogs |
| `damessage`, `damodel`, `datool`, `dastate` | Stable public contracts and reducers |
| `damodel/modeltest` | Scripted and prompt-driven predictable model doubles for offline tests and examples |
| `dabackend` | State, memory, host filesystem, namespaced store, composite, explicit local-shell, and reusable execute+upload sandbox backends |
| `dabackend/docker` | Owned local Docker sandbox with a confined workspace, hardened defaults, resource limits, and lifecycle cleanup |
| `dabackend/langsmith` | Adapter for an existing LangSmith sandbox using `langsmith-go` |
| `dabackend/runloop` | Transport-neutral Runloop devbox sandbox and explicit lifecycle/blueprint provider |
| `dabackend/daytona` | Transport-neutral Daytona sandbox with bounded session-log polling and native batch transfer |
| `dabackend/modal` | Transport-neutral adapter for an existing Modal sandbox |
| `dabackend/vercel` | Transport-neutral adapter for an existing Vercel Sandbox |
| `dabackend/agentcore` | Transport-neutral AgentCore Code Interpreter sandbox and explicit non-reconnectable session provider |
| `dasandbox` | Explicit remote-provider registry, capability metadata, setup, attach/create ownership, and bounded cleanup |
| `dabackend/contexthub` | Persistent Context Hub agent-repository files with linked-entry and parent-commit support |
| `dacheckpoint` | Saver contract and in-memory implementation |
| `daeval`, `daeval/harbor`, `daeval/clbench` | Deterministic behavioral, sandbox-benchmark, and continual-learning evaluation contracts |
| `damanaged` | Bounded managed-project loading, deployment, directory synchronization, listing, inspection, and deletion client |
| `daplugin` | Bounded plugin/marketplace manifests, materialization, lifecycle state, discovery, and component composition |
| `datalon`, `datalon/approval`, `datalon/mcp`, `datalon/mcp/oauthpolicy`, `datalon/telegram`, `datalon/whatsapp`, `datalon/cron`, `datalon/lifecycle`, `datalon/tracing`, `datalon/speech` | Experimental long-running host, channel tool approval, MCP loading/provider OAuth, bounded channels, persistent scheduler, sensitive-state retention, per-run tracing, and opt-in voice transcription |
| `browser/...` | Reusable browser WebAssembly filesystem, IndexedDB checkpoint, promise bridge, just-bash, directory-handle, and WebGPU adapters; see [`browser/README.md`](browser/README.md) |
| `dacheckpoint/sqlite`, `dacheckpoint/postgres` | Python-schema-compatible durable savers |
| `dastore`, `dastore/sqlite`, `dacache` | Namespaced data store and cache contracts and implementations |
| `daproviders/anthropic` | Direct Anthropic Messages API with hosted tools, remote MCP, and native SSE |
| `daproviders/claudeagent` | Persistent isolated Claude CLI model with caller-owned tools over MCP |
| `daproviders/openai` | Focused Responses API adapter and credential flows |
| `daproviders/nemotron` | Opt-in Nemotron harness profiles and model-output repair middleware |
| `daproviders/profile` | Explicit provider-construction profiles, model-spec resolution, provider matching, and Bedrock detection |
| `daproviders/modelconfig` | Multi-provider catalog, credentials/endpoints, model options, caller factories, and default/recent preferences |
| `daproviders/ollama` | Explicit, bounded, local-only Ollama model discovery |
| `daproviders/langsmithgateway` | Explicit provider-neutral LangSmith LLM Gateway route resolution |
| `daweb` | Opt-in SSRF-hardened HTTP request, page-fetching, and Tavily search tools |
| `davideo` | Video extraction contracts and optional bounded FFmpeg adapter |
| `daacp` | Agent Client Protocol v1 server adapter for editor integration |
| `daagentprotocol` | Agent Protocol background-subagent client |
| `daeventbus` | Opt-in bounded newline-delimited external event ingress over a private Unix socket |
| `daprofilecfg` | JSON/YAML-safe harness-profile configuration |
| `daconfig` | Canonical layered configuration, bounded owner-private file management, and typed server environment payloads |
| `daskill` | Agent Skills parsing, precedence, exact-target symlink trust, built-ins, read-only thread inspection, validation, and rendering contracts |
| `damcp` | Bounded MCP config discovery, environment resolution, and definition-bound project trust policy |
| `dasubagent` | Bounded, confined discovery and precedence for filesystem-defined declarative subagents |
| `daworkspace` | Shared workspace-instruction discovery, scoped guidance summaries, conventional directory filtering, and opt-in local environment context middleware for explicit sandboxes |
| `daserver` | Embeddable LangGraph Agent Server protocol for LangSmith Studio and SDK clients |

The graph runtime is internal. dago claims compatibility only for the Deep Agents
surface documented in [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md), not general
LangChain or LangGraph compatibility.

## Docker sandbox

`dabackend/docker` creates and owns a container from an image that is already
available to the local Docker daemon. It never pulls an image implicitly. The
image must provide `/bin/sh` and `sleep`.

```go
sandbox, err := docker.New(ctx, "my-agent-sandbox:local", docker.Options{})
if err != nil {
	log.Fatal(err)
}
defer sandbox.Close()

compiled := dago.New(chat, dago.WithBackend(sandbox))
```

By default the container has no network, a read-only root filesystem, no Linux
capabilities, `no-new-privileges`, bounded memory/CPU/PIDs, a writable `/tmp`, and
one private workspace bind mount. Set `Network`, resource fields, `User`, or
`WritableRoot` explicitly when the workload requires broader authority. Closing
the backend forcibly removes its container and its automatically created workspace.

## Examples

- [`examples/basic`](examples/basic) is a network-free invocation.
- [`examples/openai`](examples/openai) streams a live workspace summary with an API
  key.
- [`examples/studio`](examples/studio) runs a network-free agent through `dago dev`
  and LangSmith Studio.
- [`examples/shelley`](examples/shelley) contains shelley-in-dago, a full web
  application powered by dago.
  [Run it in your browser](https://semistrict.github.io/dago/).

## Security

Shell execution is never granted by a plain backend. `dabackend.LocalShell` runs trusted
host processes and is not isolation. Filesystem permissions are code-enforced and
cannot constrain shell commands, so an application must use an isolated sandbox or
omit `execute` when path-level permissions are required. See
[`docs/SECURITY.md`](docs/SECURITY.md) before exposing an agent or the web example.

The core project is MIT licensed. The copied and modified shelley-in-dago example is
Apache-2.0 licensed. See [`NOTICE`](NOTICE) for attribution and additional notices.
