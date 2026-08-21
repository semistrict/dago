# Security boundaries

The local filesystem backend performs confined operations through `os.Root`.
Traversal and symlink escapes are rejected by the rooted operating-system API rather
than path-prefix checks. Ordered allow, deny, and ask rules are enforced in code
before tool execution. Delete rules must cover the target and descendants. Prompts
and tool names cannot grant authority.

`dabackend.LocalShell` executes trusted host commands and is not a sandbox. Applications
must opt into it explicitly and should prefer Docker or a remote sandbox for untrusted
work. Its inherited file methods use `os.Root`, but `os.Root` cannot confine an
arbitrary subprocess: shell commands may use absolute paths or leave the working
directory. Command output and duration are bounded, but those limits are not isolation.
The coding-agent `--shell-allow-list` rejects substitution, redirection, variable
expansion, control characters, background execution, and any pipeline segment whose
first executable is not listed. This is an approval policy, not process isolation:
explicitly allowing an interpreter or command wrapper also allows the behavior exposed
through that executable. The `all` value deliberately disables these checks.

`dasandbox.Registry` does not dynamically import provider names, discover credentials,
or create resources during construction. Applications bind authenticated Go factories
explicitly; configuration can only select registered Go code. Setup files are
workspace-confined and size-bounded, uploaded as literal bytes to a generated remote
path, and executed without host environment expansion. Fresh resources are deleted on
setup failure and session close; attached IDs are never deleted by the client. Provider
errors and panics are reduced to stable classifications so credentials in SDK errors do
not reach users or model context.

Plugin installation is an explicit trust decision because enabled plugins may
contribute instructions, shell hooks, and MCP process or network authority. Discovery
never executes components: it copies only bounded regular files into an owner-private
cache, rejects links and special files, confines declared paths after resolution, and
returns an immutable component snapshot. State mutations are interprocess locked and
atomic; malformed or future-version state fails closed. Direct catalogs use the
SSRF-hardened HTTPS client. Repository materialization uses credential-free HTTPS,
fixed Git arguments, no shell or submodules, disabled prompts and global/system Git
configuration, bounded diagnostics, and a finite timeout. Remote refs are not signed
or immutable, so operators remain responsible for publisher and revision trust.
At process or ACP-session startup, dacode reads only enabled installations already
present in that store; it never performs plugin network discovery or updates. Plugin
skills use read-only virtual mounts and qualified names. Plugin MCP servers receive
scoped collision-resistant names, an owner-only store-confined data directory, and
plugin-specific environment additions limited to their declaration plus
`CLAUDE_PLUGIN_ROOT`, `PLUGIN_DATA`, and `CLAUDE_PROJECT_DIR`; stdio MCP processes
otherwise follow the existing MCP ambient-environment contract. Relative working
directories cannot escape the plugin root.
Plugin hook documents are failure-isolated, but valid handlers execute as trusted
unsandboxed host processes at the lifecycle boundaries dacode can faithfully project.
Disabling or uninstalling a plugin takes effect for newly created runtimes; an already
compiled agent retains its immutable snapshot until `/reload`. Reload resolves a new
dotenv overlay, credentials, web tools, shell policy, plugin snapshot, hooks, and MCP
connections before swapping the idle runtime. Failure or concurrent activity retains
the prior runtime and rolls the environment back. Reload reports key names and stable
diagnostics, never credential values. Plugin-manager errors and hook status text are
terminal-safe and bounded before entering scrollback; status callbacks cannot block or
panic hook execution.
Interactive `!` and `!!` composer commands are explicit user-authored local shell
commands, not model tool calls, and therefore do not pass through tool approval or
the shell allow-list. They run in the selected workspace with a 60-second deadline
and bounded output, but they are not sandboxed. `!` adds the bounded result to the
next model request; `!!` keeps both the command and result out of model context.

Model-driven local file and `execute` tools also use unrestricted host paths, matching
the local Dcode contract. The configured working directory is their starting point,
not an isolation boundary: absolute paths are used as-is and `/` is the host root.
Tool approvals remain the authorization boundary. Remote sandbox providers use their
own filesystem and working directory instead and do not receive local host paths.

Lifecycle hooks are trusted operator-authored host commands, not sandboxed model
tools. `dahook` never loads project hooks without an explicit session grant or an
interactive persisted workspace grant; headless use ignores persisted grants and
requires an explicit opt-in. Trust records and transcript projections are atomic,
owner-only, bounded, and keyed by canonical or path-confined identities. Hook
subprocesses inherit a sanitized environment with credential-shaped names removed;
plugins receive only the documented plugin/project variables supplied by the host.
Standard output, standard error, configuration, transcript content, deadlines, and
fulfillment history are bounded. Unix process groups and Windows kill-on-close Job
Objects terminate hook descendants; finite pipe waits prevent inherited descriptors
from blocking cancellation indefinitely. Each authenticated client session needs a
fresh, caller-delivered 32-byte-or-longer capability key. HMAC authentication covers
every interrupt request field before an atomic client-side invocation claim and any
hook side effect. In-progress claims cannot be evicted; completed and failed claims
remain in a bounded recent replay window. A separate HMAC binds the complete response
decision to its exact request, and the server authenticates it before consuming the
pending request.
Transcript projections redact credential-shaped values, authentication and cookie
headers, credential-bearing URL components, and PEM private keys. These protections
prevent ambient credential disclosure and accidental untrusted-workspace execution;
they do not make an explicitly trusted hook command safe. Run hooks with the same
care as any other local executable.

Headless coding-agent runs classify discovered MCP tools from the protocol's tool
annotations, not their names or descriptions. Only a tool with an explicit
`readOnlyHint=true` and no contradictory destructive hint can execute without
review. Every other MCP tool enters the configured approval flow; if no approval
policy exists, an execution-boundary wrapper rejects the call. MCP annotations are
server-provided hints, so this protects against missing or inconsistent declarations,
not a server that deliberately lies. Treat the MCP server itself as trusted until
project-server trust isolation is configured.

`damcp` is the project-server trust isolation boundary and must run before any MCP
transport is constructed. It reads approvals and denies only from a caller-selected
operator config path and a required process-environment lookup; callers must never
substitute values loaded from a project `.env`. Explicit denies override session,
environment, and remembered grants. Remembered approval records bind a normalized
project identity, exact server name, and canonical definition fingerprint, so command,
URL, transport, header, or argument changes ask again. Only fixed, non-interpolated
remote URLs may use `dagit`'s reciprocally validated Git common-directory identity;
local commands and all ambiguous definitions stay exact-worktree scoped. Policy reads
are bounded and reject symlinks and special files. Unreadable TOML or deny data blocks
whole-project trust while preserving only independently readable process policy; an
oversized process deny list also suppresses process grants. Writes serialize through
the store, preserve unrelated TOML data, replace atomically with mode 0600, and never
include server definitions in errors. A successful remembered approval or OAuth login
does not mutate a live tool registry: the host must schedule a reconnect once active
work is idle, and must make cancellation or deferral visible to the operator.

The non-TUI MCP login command selects only user, explicitly selected, or
definition-bound trusted project servers. Trust is decided before environment and
header interpolation, and OAuth endpoints require credential-free HTTPS. Slack and
GitHub policies match exact hostname boundaries; lookalikes use the generic MCP PKCE
and dynamic-registration policy. Browser launching uses direct process arguments and
is optional. Callback input, provider responses, device polling, tokens, and errors
are bounded; token values are never printed. Tokens are endpoint-bound, stored outside
`.mcp.json` through the private atomic mode-0600 store, and runtime reuse never invokes
an interactive flow. The caller-owned HTTP transport still owns DNS, proxy, and
certificate policy.

MCP configuration discovery keeps raw server definitions uninterpolated until after
that trust decision, so an untrusted project cannot activate commands, URLs, headers,
or environment-derived credentials merely by adding `.mcp.json`. User, project
subdirectory, and project-root sources have deterministic precedence; a winning
higher-precedence server definition shadows its lower counterpart before activation. Config files,
definitions, server counts, fields, schemas, tool counts, and returned content are
finite; links and special files are refused. Remote clients refuse redirects so custom
headers cannot cross origins, and configured tool names are server-prefixed to prevent
same-name collisions. Resolved connection values can contain secrets by design and
must not be logged, persisted, or included in model-visible diagnostics.

The GLM-5.2 terminal-stall middleware is opt-in because a tool-free response can
be intentional in an interactive session. It recognizes only the exact measured
Fireworks provider/model pair and a normalized max-token response containing one
assistant message with no tool calls or structured output. Recovery clones the
request, appends a trusted instruction, disables reasoning, and requests a tool
once; a second stalled response is returned without another retry. The forced
model choice grants no authority: normal tool lookup, argument validation,
permission approval, sandboxing, and cancellation still run before execution.
OpenRouter, Baseten, custom gateways, and near-miss response shapes are unchanged.

`daconfig` treats configuration files and process environments as untrusted operator
input. Canonical manifests reject duplicate keys and environment names at construction;
resolution bounds layer counts and values, validates types and integer ranges, and
never returns redacted values through introspection entries. The coding-agent file is
a strictly decoded version-1 JSON envelope, rejects unknown or non-persistable options,
symlinks, special files, trailing data, oversized content, and unsupported versions,
and is replaced atomically with mode 0600 after missing parent directories are created
with mode 0700. Existing caller-selected parent directories remain an operator-owned
boundary. Config-file writes deliberately reject credentials. Concurrent processes require external
serialization if they edit the same file; atomic replacement prevents partial files
but does not merge competing updates.

The `DEEPAGENTS_CLI_` prefix wins over `DEEPAGENTS_CODE_` and canonical environment
names; a present empty value deliberately shadows a lower string value. Malformed or
oversized environment values fall through rather than expanding work bounds.
`ServerConfig.Environment` contains model and local runtime settings but no API key;
its inverse parser rejects malformed booleans, JSON, paths, commands, and work limits.
Environment variables can be observable to same-user processes on some platforms, so
applications should still scope child-process environments and never add credentials
to this IPC payload. `dagent.RuntimeModel` validates and bounds model specs, isolates
concurrent invocation contexts, persists specs only in private state, and discards
resolver panic values. The required resolver remains responsible for credentials,
provider trust, caching, cancellation compliance, and network policy.

`daproviders/modelconfig` reads credentials only from the required owner-private store
and caller-supplied environment snapshot. Stored key/endpoint pairs are kept coherent,
prefixed empty values can suppress canonical values, and base URLs reject credentials,
queries, fragments, control characters, and non-HTTP(S) schemes. Parsing, status reporting, and
preference management never call provider factories or discover the network. Parameter
and profile trees have finite entry, depth, and byte limits; retry counts are bounded;
factory panics are contained; typed nil results fail; and returned factory errors redact
known credential and secret-shaped parameter values while retaining `errors.Is`
identity. Status and formatting methods expose only provider, source environment name,
authentication mode, and availability. Factories remain trusted: they receive the
credential by design and must enforce transport timeouts, cancellation, TLS, proxy,
redirect, request, and response policy. Default and recent specs are non-secret but may
reveal internal model names; their configuration file is still owner-private and
atomically replaced.

`daenv` treats a project `.env` as untrusted repository input. Shell values always win;
the nearest project file can only fill missing safe names, and the user-global file can
only fill names still absent. Dynamic-loader, interpreter, shell-startup, executable
path, and askpass controls are rejected from every dotenv layer. Proxy, TLS,
tracing-endpoint, MCP-trust, automatic-review, and terminal-identity controls are also
rejected from project files so a checkout cannot weaken the user's policy or redirect
credentials. Ignored diagnostics contain key names and fixed reasons, never values.
Files must remain the same regular non-link inode through open and are bounded along
with discovery depth, lines, keys, values, and aggregate output. The Go parser supports
plain, single-quoted, and escaped double-quoted assignments but intentionally does not
expand variable references, preventing a repository value from copying a launch-time
secret into a second environment name.

`daproviders/ollama` is opt-in local discovery, not a general endpoint client.
Construction performs no I/O; only an explicit `Discover` call sends one GET through
the required caller-owned transport. Endpoint validation rejects credentials,
queries, fragments, non-root paths, non-HTTP(S) schemes, wildcard addresses, remote
hosts, and DNS names; exact `localhost` is rewritten to `127.0.0.1` before the
request. The client does not follow redirects itself or attach authorization, bounds
time, response bytes, model count, and name size, rejects padded/control-character
names, sorts and deduplicates output, preserves cancellation, and sanitizes transport
errors and panics. A caller-supplied transport remains a trusted execution boundary
and should not rewrite or proxy literal-loopback requests.

`daproviders/langsmithgateway` performs no credential discovery, model request,
or other I/O itself. Its required caller factory receives the gateway key only at
the final construction boundary; the resolver keeps that key private and removes
factory error and panic values from returned errors. Endpoint inspection never
returns the credential. Remote endpoints require HTTPS, while plain HTTP is
limited to literal loopback and exact `localhost` is rewritten to `127.0.0.1`.
User information, query strings, fragments, traversal, unsafe path segments,
unbounded model specifications, and unbounded provider maps are rejected.

The supplied factory remains a trusted boundary: it owns provider selection,
TLS, proxying, redirects, retries, request and response bounds, and actual network
access, and it must honor cancellation. A factory can observe the credential and
endpoint by design. Applications should avoid logging its arguments and should
not derive provider routes from untrusted configuration without their own policy.

`daeventbus` is disabled unless an application explicitly constructs and runs a
source with a required sink and absolute path. It creates or requires an
owner-private parent directory, forces the Unix socket to mode 0600, rejects
relative, unclean, root, oversized, shared-directory, symbolic-link, regular-file,
and active-socket targets, and removes a stale socket only after a local refusal.
Shutdown compares the bound file identity before unlinking, so a replacement
entry is preserved. Windows fails with `ErrUnsupported`; it never falls back to
TCP or another remotely reachable transport.

The source parses only bounded UTF-8 JSON lines, limits simultaneous clients and
events per connection, applies finite idle, sink, and write deadlines, closes
clients on cancellation, and returns static bounded NACK text. Sink errors and
panic values are not reflected to peers. The socket does not authenticate beyond
local filesystem access: any process able to enter its private directory and open
the socket can submit an event. The caller-owned sink is the authority boundary
and must validate command policy, treat bypass as an untrusted hint, honor its
context, and avoid granting shell, model, filesystem, or network access merely
because transport validation succeeded.

`datalon` treats channel adapters, schedulers, and agent runtimes as application-owned
trust boundaries. The host validates and bounds routing identifiers and message sizes,
uses channel-qualified conversation IDs, copies top-level metadata before adding trusted
host fields, and gives every blocking call a cancellation context. Adapters and runtimes
must honor that context and must authenticate their own remote peers; the host cannot
forcibly terminate a nonconforming implementation. Per-assistant state directories are
created with owner-only permissions, but the configured workspace is deliberately an
agent capability rather than a filesystem sandbox. Applications should pair untrusted
work with an isolated backend and keep channel credentials outside message metadata.

`datalon/approval` is explicitly experimental convenience HITL, not a complete
authorization, channel-administration, sandbox, or multi-tenant security boundary.
Anyone able to speak as an allowed channel operator may authorize the runtime's local
and MCP capabilities, and approval prompts disclose bounded tool names and arguments
to that channel. Authenticate channel peers before they reach `Host`, isolate risky
tools, and do not treat the reply prompt as a replacement for server-side policy.

The environment overlay is bounded, treats names as exact values rather than glob
patterns, and is applied after caller rules so a listed name forces approval even when
its base entry is false.
Apply the resulting rules at every execution boundary: dago inherits them into its
built-in general-purpose and declarative subagents, but separately compiled and remote
subagents must be configured independently. Runtimes must resume every approval
interrupt through `approval.ResolveInterrupt` or call `Policy.Authorize` immediately
before executing a directly managed local or MCP action. Handlerless scheduled work,
handler errors, invalid decisions, cancellation, and the finite host deadline reject
the gated call.

Filesystem-defined subagents are executable prompt and model configuration. Discovery
uses confined directory handles, refuses linked roots and definitions, validates file
identity across open, and bounds directories, files, fields, and resolved counts.
Project definitions intentionally override the selected profile's user definitions;
therefore trust `.deepagents/agents` to the same degree as project instructions. A
custom model name selects a caller-authenticated model but never carries credentials
from the definition. Discovered agents inherit the main backend, tools, memory,
middleware, approval rules, and sandbox boundary.

The host stores pending approvals only in memory and allows at most one per exact
channel conversation. A reply is consumed only when its normalized first token is a
supported approve/deny word or emoji and its sender exactly matches the initiating
sender when one was supplied. `/stop` retains precedence and clears the pending wait;
shutdown clears all waits. Spoofed senders, unrelated text, expired replies, and
duplicates do not enter the approval channel and continue through ordinary serialized
message handling. Pending decisions are deliberately not durable across restart.

Fleet imports accept only a caller-selected regular zip file and an explicit assistant
state directory. The importer validates every archive name and file type before writing,
rejects traversal, absolute and Windows-drive paths, duplicate entries, symlinks, and
special files, and enforces finite compressed, uncompressed, per-file, entry-count,
compression-ratio, and tool-count bounds. Materialization occurs in a private sibling
staging directory, then refreshes only fixed managed paths with rollback while rejecting
linked or special existing targets; unrelated runtime state is preserved. Generated MCP
configuration contains only sanitized HTTP(S) endpoints, OAuth metadata, and validated
tool names. URL credentials, queries, fragments, and secret-shaped path components are
removed, and raw Fleet `tools.json` and `config.json` are never copied into runtime state.

`datalon/mcp` treats the selected user configuration and local stdio commands as
operator-trusted authority. Files are regular, strictly decoded, and bounded; remote
URLs reject credentials and fragments and require HTTPS except for literal loopback;
headers, environment entries, arguments, server/tool counts, schemas, descriptions,
and results have finite limits. Remote redirects are rejected, preventing configured
headers or OAuth bearer tokens from crossing origins. The caller-owned HTTP client
still owns DNS, proxy, certificate, and public-network policy; supply an SSRF-hardened
client when server URLs or OAuth metadata are not fully trusted. MCP tool annotations,
descriptions, results, and structured content remain server-controlled model input.

OAuth uses PKCE through the protocol SDK, exact state validation, a fixed loopback
redirect, dynamic client registration, and explicit paste-back interaction. No local
listener or browser process is created. Tokens are keyed by a SHA-256 digest of server
identity and endpoint, stored only through the required caller store, and atomically
written mode 0600 below a mode-0700 directory by the built-in store; symlinked and
special token files fail closed. Error output never includes token values. The CLI
keeps tokens outside `.mcp.json`, but the authorization URL and configured endpoint
remain sensitive operational metadata and should not be logged publicly.

The optional provider-policy service matches provider hostnames at label boundaries,
so lookalike domains receive only the generic standards-based policy. It refuses
redirect following, credential-bearing or non-HTTPS remote endpoints, non-exact
loopback callbacks, non-HTTPS authorization pages, malformed Slack workspace IDs,
and GitHub verification URLs outside `github.com`. Provider responses, callbacks,
tokens, device lifetimes, polling intervals, and poll counts are finite; cancellation
interrupts both authorization serialization and device polling. Device codes and
access or refresh tokens are never returned in errors or logged by the package. The
caller-supplied HTTP transport still owns DNS, proxy, certificate, and IP-range policy,
and the token store remains a credential-bearing trust boundary.

Cron prompts, origin identifiers, delivery errors, and schedules are durable local
state and may contain sensitive operator data. `datalon/cron` confines them to the
explicit assistant directory, rejects symlink or special-file job records, writes
atomic mode-0600 files below a mode-0700 directory, validates a versioned bounded
schema, scopes agent-facing edits and removals to the originating channel conversation,
and claims a due interval before invoking the agent. Scheduler event logs contain job
IDs, names, origin conversation IDs, and bounded errors; configure their destination
accordingly. A scheduler callback still owns model credentials, result delivery, and
remote authentication, and it must honor cancellation for shutdown bounds to hold.

`datalon/lifecycle` requires the per-assistant state root and cron store
positionally and performs no I/O during construction. Cleanup rejects a linked,
non-directory, or filesystem-root state path, opens it through `os.Root`, and
walks only fixed or statically validated relative artifact roots. Entry count,
depth, cron count, per-file bytes, selected bytes, policy count, and report size
have finite defaults. The complete walk must succeed before any file deletion;
symlinks, special files, read errors, unexpected replacements, and exceeded
limits fail closed. Cleanup secures managed directories to mode 0700 and regular
files to mode 0600, rechecks file identity immediately before atomic unlink,
and removes only empty nested directories. Completed cron jobs are pruned under
the store lock through its validated, flushed atomic replacement path.

Dry-run and deletion reports deliberately omit cron prompts, raw IDs, paths,
filenames, contents, credentials, and trace payloads. Their stable audit
references are truncated SHA-256 digests of a type-qualified local identifier;
they permit correlation but not recovery of high-entropy names. The pinned safe
defaults never delete channel authentication/session state and manage no remote
LangSmith data. Local channel or tracing exports require an explicit confined
policy, and credential-bearing session cleanup additionally requires the exact
static acknowledgement. Run retention before channel and scheduler background
writers. The manager serializes its own calls and the supplied cron store
serializes cron updates, but arbitrary external writers remain outside that
coordination. Cancellation is checked throughout; already completed atomic
replacements or unlinks remain deleted and are marked in the returned report.

`datalon/tracing` is disabled unless an application explicitly wraps its runtime;
the environment helper additionally requires both a truthy tracing flag and a
non-empty API-key variable. Neither the provider-neutral wrapper nor the LangSmith
adapter accepts, reads, or emits the credential itself. The caller-owned client
retains responsibility for credential loading, endpoint, TLS, proxy, retry, flush,
and shutdown policy.

The remote provider receives bounded request and response text, bounded errors,
assistant and conversation identifiers, tags, and a JSON-cloned subset of request
metadata. Treat all of those fields as potentially sensitive. Trace start and finish
failures are reported only through the optional callback and never replace the
runtime result; panic values are not disclosed. Completion receives a finite
deadline after runtime cancellation, but an untrusted sink that ignores its context
can still block the caller and must not be installed.

Managed tracing additionally snapshots canonical tracing/project/key variables
before applying the agent project and returns a separate restored subprocess
environment, avoiding global mutation and accidental project inheritance. Empty
prefixed key and flag overrides suppress canonical or stored values; a truthy flag
without a credential, a non-default confined endpoint, or a valid replica target is
disabled as orphaned. Remote endpoints require HTTPS, while HTTP is limited to literal
loopback. URL credentials, queries, fragments, malformed replica names, oversized
environment data, and unbounded limits fail closed.

Resolved API keys and replica-ingestion keys are retained only inside the immutable
configuration, passed to the required caller sink factory, and replaced in trace
inputs, outputs, errors, metadata keys, and metadata values. Provider factory and
managed sink failures are reduced to stable non-secret errors. Exact-value redaction
does not discover unrelated secrets; applications should still avoid tracing secret
material and may enforce stronger provider-side anonymization. Derived environments
contain credentials and must never be logged. The provider factory and credential
store remain credential-bearing trust boundaries.

Thread URL resolution accepts only bounded, credential-free HTTPS project URLs,
escapes bounded thread identifiers, coalesces concurrent lookups, caches only success,
and propagates cancellation through the required caller lookup. It neither launches
a browser nor grants network authority beyond that injected lookup.

`datalon/telegram` requires the bot token and caller-owned HTTP client positionally,
never reads credentials from the environment, and performs no request during
construction. The Bot API carries tokens in URL paths, so client tracing, redirect,
proxy, and error policies must not record or forward complete request URLs. The
official HTTPS API is the default; a custom API base and HTTP transport are trusted
application choices. Per-request timeouts, response/request/webhook bodies, update
batches, text, metadata values, media size declarations, retry delays, error details,
and offset files have finite defaults and hard maxima. Offset replacement is atomic
and owner-only, but its caller-selected directory remains an operator-owned trust
boundary.

Inbound self exposure accepts authenticated bot-authored traffic and otherwise
rejects messages until the caller supplies operator IDs.
Allowlist mode distinguishes private-user IDs from channel-chat IDs; group and
supergroup messages never dispatch. Open exposure requires a deliberate constructor
acknowledgement because arbitrary senders then reach the operator's runtime and tools.
Webhook mode additionally requires a positional secret and constant-time header
comparison, owns no listener, and never polls; the application must enforce HTTPS,
routing, request-source policy, listener limits, and Bot API webhook registration.
Long polling authenticates with `getMe` before starting, persists offsets only after
a batch is handled, caps provider retry delays, and stops through request context
cancellation. A caller HTTP implementation that ignores contexts can still delay
shutdown. Media is exposed only as bounded Telegram metadata in this adapter; it is
not downloaded or granted filesystem authority.

`datalon/whatsapp` requires a caller-supplied transport and stable session
directory positionally and performs no I/O during construction. Its built-in
HTTP transport accepts only bearer-authenticated loopback origins, pins
`localhost` without DNS, disables proxies and redirects, bounds request and
response bodies, and applies a finite deadline. A custom transport must already
be authenticated, honor context cancellation, and enforce byte limits before
allocation. The Go channel validates returned payloads again, but cannot reclaim
memory already allocated by a nonconforming transport. The application owns the
packaged Node process, Chrome, restart policy, logs, and bridge-token delivery.

The bridge rejects non-loopback listeners, uses constant-time bearer comparison,
and places persistent pairing credentials and downloaded media in mode-0700
directories with mode-0600 media files. JSON bodies, queue count and aggregate
bytes, message fields, batch sizes, errors, send chunks, and decoded media are
bounded; the WhatsApp-specific media ceiling is always clamped to 64 MiB.
Outbound local media must resolve beneath the caller-selected root, is copied to
the bridge media directory, and rejects symlink escapes, special files, MIME/type
mismatches, and size changes during copying. Inbound paths are likewise confined
before entering model-visible metadata. Open exposure requires an exact static
acknowledgement; self and allowlist modes remain the safer defaults. Session data
and bridge credentials must never enter prompts, messages, or diagnostic output.

`datalon/speech` treats channel media paths and transcripts as untrusted. It rejects
links and special files, opens media beneath a rooted parent, enforces size before and
after opening, and gives ffmpeg only a private mode-0600 copy, closing the path-swap
window. ffmpeg and the optional Python Transformers process receive fixed direct argv,
finite deadlines, serialized local inference, private temporary WAVs, and bounded
stdout/stderr; operators remain responsible for executable and model provenance,
optional model downloads, GPU resource limits, and the Python environment. The remote
adapter requires its HTTP client, key, and model positionally, rejects redirects, uses
an HTTPS base, and bounds multipart input, responses, errors, and duration. A custom
HTTPS base is explicitly trusted with the API key. Voice transcripts remain
attacker-influenced model input, and applications should keep best-effort error details
out of user-visible channels unless they are rendered safely.

The coding agent's first-enable Auto notice is an education boundary, not an
authorization control. Its versioned acknowledgement shares `approval.json` with the
YOLO policy acknowledgement and per-thread approval modes under the private state
directory. Thread records use SHA-256 keys rather than raw thread identifiers.
Read-merge-write
updates use process and operating-system file locks plus an atomic mode-0600
replacement, so concurrent acknowledgements preserve each other's fields. Missing,
malformed, oversized, non-regular, or outdated records never count as acknowledged.
Esc from the Auto notice returns to Manual without writing; failure to save after Enter
leaves Auto active for that session but causes a later enable to retry. YOLO is
fail-closed: it remains inactive until the current warning policy is saved successfully;
`m` chooses Manual and Esc keeps the previous mode. Approval rules read the active
thread's durable mode at the server execution boundary for main and delegated-subagent
tools. Missing, invalid, corrupt, or unreadable mode state is Manual; only a validated
YOLO record bypasses a gate. TUI changes are applied after the durable write succeeds.
If a live change back to Manual cannot be persisted, active work is cancelled and new
runs are blocked rather than trusting divergent UI state. Tool enforcement remains
independent of the Auto education preference.

Manual rejection reasons are trusted user input. Before a reason reaches either the
durable model resume or the local audit row, the terminal client trims it, converts
terminal controls and deceptive Unicode to visible markers, and bounds it to 4,000
characters and 16,000 UTF-8 bytes. The model receives an explicit
`User rejected the tool call with reason:` frame; a blank field remains a bare
rejection. This neutralization prevents a reason from writing terminal control
sequences, but it is not an authorization boundary and the reason remains part of the
durable conversation. Approval deferral reduces accidental `y`/`n` decisions while a
draft is being typed: a generation-bound idle timer reveals the prompt after two idle
seconds, with a 30-second ceiling. Deferral does not weaken server-side approval
enforcement or authorize the pending tool. Auto review has its own cancellable turn
context; cancellation is checked again before a reviewer decision can resume the
gated tool. Drafts submitted during review or deferral remain queued behind the
approval, including when the approval continuation itself ends with an error.
Changing the durable live mode also reconciles the pending request: Manual cancels
and ignores an in-flight reviewer result, Auto starts review for an already-visible
or deferred Manual request, and YOLO follows its separately acknowledged gate.

Approval previews are bounded, terminal-neutralized views of the proposed tool
arguments, not an execution sandbox or a guarantee that filesystem state will remain
unchanged before the approved call runs. Write and edit previews suppress contents for
recognized credential filenames, and the matching pending tool row is redacted so the
same payload does not leak through scrollback. This filename heuristic intentionally
fails closed for malformed paths but cannot identify every secret-bearing file; users
must still treat terminal transcripts as potentially sensitive and rely on server-side
approval enforcement for authorization.

Transcript presentation never grants tool or filesystem authority. Assistant Markdown
is parsed only into terminal styles after control-character neutralization; links are
displayed as inert label-and-URL text, and no HTML is evaluated. User messages retain
at most fixed head and tail previews until explicitly expanded. Tool output, arguments,
skill bodies, and inline write/edit diffs have finite line, character, and byte limits;
recognized credential paths show only a redaction notice. The `/line-numbers` setting
is captured when a diff row is created so a later preference change cannot silently
rewrite prior scrollback. Virtualization removes old rows only from the active render
window, not from checkpoints or memory: PageUp can hydrate retained content, so it is a
performance feature and must not be treated as deletion or redaction.

Interactive `ask_user` answers are trusted user input delivered to the paused tool and
persisted in the durable model transcript. The terminal keeps completed question rows
collapsed to a fixed success or failure summary, and automatic-review transcript input
uses the same fixed summary instead of the answer-bearing tool result. This prevents a
review classifier or casual collapsed-screen view from receiving the answer, but it is
not secret storage: checkpoint readers and the active model can access the authoritative
Q&A transcript. Agent-authored questions and choices are rendered with terminal control
characters neutralized, and cancellation removes the pending interaction without
delivering partially entered answers.

Theme configuration is presentation-only and never grants instruction or execution
authority. The loader reads at most one MiB from a regular config file, ignores invalid
themes without printing their values, accepts only bounded names/labels and exact
`#RRGGBB` custom colors, and treats unknown forced themes as the safe built-in default.
Global and per-terminal selections are serialized within the app and atomically replace
the owner-private config file while preserving unrelated TOML sections. ANSI themes
deliberately defer foreground and background colors to the terminal; a malicious or
misconfigured terminal palette can reduce readability but cannot change tool policy.

`dabackend/docker` creates and owns a container from an explicitly selected local
image. Its defaults disable networking, drop all Linux capabilities, enable
`no-new-privileges`, make the root filesystem read-only, limit memory (without
additional swap), CPU, and PIDs, and mount only its dedicated workspace. Timeout or
cancellation restarts the container
to ensure the command is terminated while preserving workspace files. Docker remains
a host-privileged isolation boundary: daemon configuration, image provenance, kernel
security, and any explicitly relaxed options remain the operator's responsibility.
The adapter never mounts the Docker socket and never pulls an image implicitly.

`dabackend/runloop` receives an already authenticated remote client from the
application, bundles no vendor SDK, and never reads an API key. Constructing a
backend only attaches local behavior to a stable devbox ID; remote creation and
shutdown occur solely through explicit provider calls. Command output, native
uploads and downloads, transfer
batches, error details, and blueprint pagination are bounded, and every blocking
transport operation carries the caller's context. The transport must enforce the
supplied byte limits before allocating or returning data. Defensive return checks
prevent oversized data from entering graph state, but cannot recover memory that a
nonconforming transport already allocated. The remote image, shell, network, vendor
service, client authentication, retries, and server-side process termination remain
operator-controlled trust boundaries. The default blueprint Dockerfile tracks the
upstream `python:3` behavior; production callers should provide a digest-pinned image.

`dabackend/daytona` also receives a caller-authenticated transport and bundles no
vendor SDK. It creates no sandbox resources: each command creates only a scoped
session inside the caller-selected stable sandbox ID, polls through the caller's
context, and makes a separately bounded cleanup attempt after success, failure,
timeout, or cancellation. Command logs, polling delays, file sizes, batch sizes,
error details, and native response mappings are finite and validated. The transport
must enforce the supplied bounds before allocation. Authentication, retries, remote
image and network policy, server-side process termination, and sandbox deletion stay
with the application and service.

`dabackend/modal` likewise receives an already authenticated caller transport,
bundles no vendor SDK, reads no credentials, and never creates or deletes remote
resources. It invokes the pinned adapter's explicit `bash -c` argv boundary, rejects
non-absolute native file paths, bounds command output, file sizes, transfer batches,
and error details, and propagates caller cancellation. The transport must enforce the
supplied bounds before allocating or returning data. The remote image, shell,
network, vendor service, authentication, retry policy, and server-side process
termination remain operator-controlled trust boundaries.

`dabackend/vercel` follows the same caller-authenticated, SDK-free boundary. It
starts only the pinned adapter's explicit `bash -lc` argv shape, applies a local
deadline even when the provider wait primitive cannot, and makes a bounded
best-effort kill after timeout or caller cancellation. A transport must make its
wait return after that kill; otherwise its wait goroutine may remain until the
remote command ends. Command logs, error details, file sizes, and transfer batches
are bounded, and non-absolute paths are rejected before transport calls. Remote
resource creation, credentials, image provenance, shell and network isolation,
retries, billing, and guaranteed server-side termination remain operator concerns.

`dabackend/agentcore` receives an already authenticated caller transport, bundles
no AWS SDK, performs no credential discovery, and contacts no service during
construction. Its provider creates only fresh Code Interpreter sessions, refuses
reconnection, caps locally starting and active sessions, and makes a separately
bounded best-effort stop when start fails or its caller cancels. Command output,
stream events, error details, file sizes, transfer batches, start, stop, and command
duration are finite by default; every transport operation receives the caller's
context. The transport must enforce the supplied bounds before allocation and must
authenticate and authorize every session ID. Direct transfer paths preserve the
upstream virtual-path behavior, including parent components, so the remote service's
filesystem policy remains authoritative. AWS credentials and region policy, remote
runtime and network isolation, service-side session expiration, retries, billing,
and guaranteed process or session termination remain application and service trust
boundaries.

`daeval/harbor` does not create a sandbox, start a process, load credentials, or
contact a registry or reporting service. It runs only through the `Runner` supplied
to `NewBenchmark`; that implementation owns isolation, credentials, images, network
policy, verifier integrity, cleanup, and server-side termination. The harness passes
benchmark and per-trial deadlines, but a runner that ignores context can still block
its caller. Task text, metadata, check count, task count, trajectory structure,
result bytes, failure details, and elapsed time have finite defaults. Runner panics
become generic errors so panic values cannot enter reports. Exit-code extraction
examines tool observations structurally and never scans assistant prose, preventing
model-authored text from falsely converting a capability failure into infrastructure
noise. Hosted tracing and dataset publication remain application-owned operations.
The ContextBench and DRBench adapters accept already-acquired, agent-visible records;
they never download corpora, resolve images, or run a verifier. Their public record
types cannot hold ground truth, verifier prompts, or passwords. DRBench application
usernames use a restricted form before prompt interpolation, task IDs are single
validated identifiers, record text and lists are bounded, and batch adaptation is
cancellation-aware and capped at 1,000 records. Adapter `network_mode` metadata is an
instruction to the caller-owned runner, not network enforcement: the runner remains
responsible for applying ContextBench's restricted egress and DRBench's deliberately
public research policy without exposing host services or credentials.

`daeval/scorecard` receives model execution through a required caller-owned `Runner`.
Its `Model` values contain only a validated stable ID and optional provider label;
credentials, endpoints, request options, and SDK clients have no scorecard fields and
must be resolved out of band by the runner. Every model receives the same ordered task
set. Model, task, and matrix bounds are checked before execution so an oversized or
unfair comparison makes no runner calls; overall, per-model, and per-trial deadlines
then flow through the Harbor cancellation boundary. Runner panics and details remain
subject to Harbor's generic panic conversion and bounded failure reporting. Reports
contain no clocks, durations, credentials, or hosted experiment identifiers. Scorecard
rankings measure supplied verifier outcomes; they do not establish model or provider
trust, sandbox integrity, statistical independence, or production suitability.

`daeval/clbench` performs no provider initialization, network access, process
execution, credential lookup, or host filesystem access. Its required `Factory`
and schema-specific `Agent` own those authorities and must honor the supplied
context. The system passes only cloned prompt and in-state file values, seeds a
single `/memory/AGENTS.md` strategy file, treats that memory as untrusted in its
fixed system prompt, and never supplies secrets. Returned actions must be bounded
JSON objects; file paths, encodings, per-file and aggregate bytes, schema count,
turn count, usage records, tokens, error details, and turn duration have finite
defaults. Invalid output is rejected atomically, returned and input maps cannot
mutate retained memory, and panic values are discarded. A caller-provided agent
can still use whatever model, tools, environment, or network the application gives
it; isolation and credential policy remain the caller's responsibility.

The safe checkpoint serializer is an allowlist. It does not implement pickle,
dynamic imports, arbitrary constructors, or callable execution. OAuth tokens are
stored only when the caller provides a path, using an atomic private file with mode
0600. Error bodies are bounded and credentials are not logged.

The subscription OAuth helper uses PKCE, verifies the callback state, binds its
temporary callback listener to loopback, honors cancellation, refreshes expired
credentials, and stores tokens only in its own caller-selected file. It does not read
or reuse another application’s credential store. API-key and OAuth credentials remain
adapter concerns and are never placed into graph state or checkpoints.

`dacredential` stores provider and service secrets only at the caller-selected
`auth.json` path. Reads reject linked, special, oversized, corrupt, or unsupported
files; writes use a private temporary file, durable atomic replacement, mode 0600
for the file, and mode 0700 for its state directory. Credential, resolution, and
error formatting never includes secret values. Store operations and lock waits are
cancellable and bounded. Environment resolution is performed only through the
caller-supplied lookup and validates the same finite secret shape as disk records;
stored values take precedence over prefixed and canonical environment values. The
store is not an OS keychain and does not encrypt secrets against the local account,
so applications should place its state root on a trusted local filesystem and avoid
copying resolved values into logs, graph state, prompts, or checkpoints.

The Agent Protocol client sends credentials only to its configured origin, refuses
redirects, bounds response bodies, and escapes thread/run identifiers. Its automatic
API-key lookup follows the upstream precedence (`LANGGRAPH_API_KEY`, then
`LANGSMITH_API_KEY`, then `LANGCHAIN_API_KEY`); use a custom runner when a different
credential policy is required.

The optional `daweb` tools grant outbound network authority only when an application
constructs and supplies them. Their client accepts only HTTP and HTTPS URLs without
embedded credentials, resolves every initial and redirected hostname, rejects any
answer in private, loopback, link-local, carrier-grade NAT, documentation,
benchmarking, multicast, reserved, unspecified, IPv4-mapped, or private 6to4 space,
and pins the connection to the complete validated answer set. It does not honor
environment proxies because a proxy could resolve the destination outside that
boundary. Cross-origin redirects discard caller headers and cannot forward request
bodies; Tavily requests refuse redirects entirely so their API key remains bound to
the fixed Tavily origin. Request data, response headers and bodies, rendered page
text, redirect count, and elapsed time are finite. This prevents access to local
services; it does not make public web content trustworthy, so fetched text remains
untrusted model input. Dacode binds `fetch_url` at startup and prefers provider-hosted
web search when the resolved model advertises it, including OpenAI and Anthropic
integrations. OpenAI's hosted `web_search` is enabled by default. When the resolved
model lacks hosted search, dacode adds a local `web_search` only when its
stored-over-environment Tavily credential resolves. If hosted search is available, the
local fallback is removed. The Tavily key stays in the tool closure, is never added to
graph state or prompts, and each fallback search requires the ordinary tool-approval
decision unless the active approval mode bypasses it.

The Claude CLI provider creates one loopback-only MCP endpoint with an unguessable path
and bearer authority for each persistent CLI process. That server exposes schemas, not
tool implementations; a selected call blocks until the outer agent loop supplies its
result. The child runs in an empty workspace with explicit empty settings and setting
sources, and disabled built-in tools, browser, skills, slash
commands, and suggestions. The child retains the authenticated user home because the
subscription login is selected through the CLI's local Keychain state; an installed-binary
test verifies that user `CLAUDE.md` content and hooks still do not load. Conversation
reconstruction writes only to the unique temporary-workspace project entry in Claude's
native JSONL format and is used only after a process restart. Process replacement or
explicit close removes the endpoint, workspace, and project entry. Ambient Claude
authentication remains an upstream CLI/keychain boundary, and administrator-managed
policy cannot be overridden by the child process.

`dago dev` is an unauthenticated local development server. It binds to loopback by
default, allows the hosted Studio origin and loopback browser origins, and places its
generated wrapper plus SQLite state under `.dago_api`. Do not bind it to a shared or
public interface. Environment values are passed to the compiled application process;
they are not returned by the Agent Server API, but application code and model tools
still have whatever access the host process grants them.

The dacode local-development companion is a separate, explicit process boundary.
`--local-dev-server` requires an absolute executable and every argument is passed
directly without a shell. Readiness accepts only a credential-free loopback HTTP
origin and an absolute same-origin path. The child does not inherit the ambient
environment: it receives a bounded typed `daconfig.ServerConfig`, a minimal platform
baseline, and only additional non-secret names explicitly selected with
`--local-dev-inherit-env`. Credential-shaped and process-loader names are rejected.
Model credentials therefore remain in the parent process unless the executable has
an independent credential source the operator intentionally configured.

Startup, probes, log tails, graceful shutdown, forced termination, arguments,
environment values, and diagnostics are finite. Diagnostics remove terminal controls,
URL userinfo, bearer values, and common secret assignments. Cancellation and startup
failure reap the process tree; Unix launches a dedicated process group and Windows
uses a new process group plus tree termination. Restart is serialized with startup
and close, and a terminal close prevents an in-flight restart from spawning a new
child. The endpoint still exposes whatever service the operator-selected executable
implements, so keep it loopback-only and treat that executable as trusted code.

`dago init` writes only below the selected current directory and accepts a single
bounded project-name component. Existing targets require explicit `--force`; linked
targets, linked managed directories, and linked or special starter files fail closed.
Force refreshes only the fixed starter paths and preserves unrelated content. Run it
only in an operator-controlled parent directory: concurrent local replacement of a
validated file remains outside the scaffold command's trust boundary.

`damanaged` sends its required API key only to one caller-selected HTTPS origin and
refuses redirects. The `dago agents` transport also disables environment proxies;
custom clients retain responsibility for DNS, TLS roots, dial policy, and connection
reuse. Responses, pages, cursors, errors, retries, and request time are finite, and
IDs cannot alter request paths. List/get output is provider-controlled and may contain
sensitive project metadata. Deletion requires an interactive acknowledgement unless
the operator explicitly supplies `--yes`; remote deletion is not recoverable here.

Managed deployment treats every project file as untrusted local input. The loader
uses a confined root, accepts only UTF-8 regular files and real directories, rejects
links and special entries, and bounds each file, total bytes, entries, children, and
depth. `agent.json`, tools, skills, subagents, backend scope, and permissions are
validated before authentication or network work. Dry-run performs neither. Remote
directory deletion is restricted to the fixed managed roots; unrelated provider
files are preserved.

The durable remote binding is stored outside the checkout in a private directory,
keyed by a SHA-256 digest of the absolute project path and authenticated endpoint,
and replaced through a mode-0600 temporary file. Checkout-controlled state is never
read. A declared `agent_id` requires confirmation and fails closed on a missing
remote; stale cache-only IDs may create a replacement. Deployment can mutate the
remote before a local state-write failure is reported, so operators must reconcile a
reported partial failure before retrying. `--reset` and `--yes` are explicit risk
acknowledgements, not authorization substitutes.

Before agent creation, deployment persists a stable non-secret recovery key and sends
that key in the remote `extras` record and the standard `Idempotency-Key` request
header. The key is a deterministic digest of the authenticated endpoint plus the
required project-owned `extras.dago_deployment_id`, so separate checkouts and state
roots use one server-side creation identity without conflating equal display names.
A lost create response or failed final state
replacement is recovered by exact key through bounded agent listing and inspection.
The client may repeat a failed create once with the same idempotency key; after that
invocation, an unresolved pending creation blocks a fresh create until it appears or
the operator explicitly reconciles it and uses `--reset`. Server-confirmed 4xx
rejection clears the pending intent because that response is a definitive non-creation
result. Windows
state replacement uses the operating system's replace-existing, write-through move so
the prior binding is not deleted before its replacement succeeds; Unix replacements
flush the containing directory. A bounded per-project operating-system lock spans
recovery, remote mutation, complete directory synchronization, and final state write,
preventing concurrent local deploy processes from reserving different creations.
The managed service must honor the idempotency header for simultaneous first creates
from different hosts; the client additionally detects multiple matching remote markers
and fails closed rather than choosing one.
Reset refuses to reuse that identity: the operator must rotate the project-owned ID
before requesting reset, making the new remote creation explicit and recoverable from
other checkouts. When the prior identity has an unresolved pending create, rotated reset
first replays that original identity and idempotency key to a definitive outcome, then
durably records the replacement key before creating its new agent. A declared agent ID
cannot replace an unresolved pending-create marker unless reset was explicitly requested
after reconciliation. Before patching a declared
target, deployment verifies both that the target is not bound to another identity and
that the current identity is not bound to a different remote agent. An unbound declared
target is rejected because claiming it with an ordinary patch would not be atomic across
machines; it must first acquire the exact binding through an authoritative server-side
workflow.

Managed MCP registry commands send explicit header values to the authenticated
workspace service but never print them; inspection output replaces stored header
values with a fixed marker. Server and tool URLs must be HTTPS without userinfo or
fragments, headers use valid bounded HTTP field syntax, names/IDs cannot inject paths,
redirects remain disabled, and remote text is terminal-sanitized or JSON-escaped.
Tool descriptions and schemas are provider-controlled data, not trusted policy.
Name/URL resolution fails on ambiguity, and registry deletion requires confirmation
unless `--yes` is explicit. OAuth registration stores no token locally; connecting a
provider account is a separate authorization boundary.

The OAuth connect command accepts only a provider-returned HTTPS verification URL
without userinfo, prints it with terminal controls neutralized, and launches the
platform URL opener through direct arguments rather than a shell. `--no-browser`
keeps that action manual. Scopes, IDs, wait durations, total timeout, response sizes,
poll cadence, status vocabulary, and cancellation are finite. Tokens are held by the
managed service and never returned to or persisted by this client. The verification
URL and provider/session IDs are still sensitive operational metadata and should not
be copied into public logs.

Video decoding is disabled unless the application supplies a `VideoExtractor`.
The optional FFmpeg adapter treats its configured executable as trusted deployment
configuration, passes video bytes through standard input, applies a context deadline,
and bounds input size, frame count, decoder output, emitted image data, and error
output. It never forwards the original video to the model. Use a separately isolated
decoder process when media itself is untrusted.

The example web application is single-user software and does not add authentication.
Its native server refuses non-loopback TCP listeners. An authenticated reverse proxy
may connect to that loopback listener; `--require-header` is only a defense-in-depth
proxy integration and is safe only when the proxy removes caller-supplied copies of
the header. Terminal and local-shell paths execute trusted host commands and are not
a substitute for sandbox isolation.

Repository guidance and repository skills are ignored by default because a checkout
may be unreviewed. `serve --trust-workspace-guidance` opts into those prompt inputs
after the operator has reviewed them; user-installed and built-in skills remain
available without that flag. Tool authorization is enforced independently of prompt
content.

`dacode doctor` is offline and never reads or prints credential contents. It
reports only whether an API key or saved sign-in exists, reduces configured
provider URLs to their scheme and host, escapes terminal controls, and bounds
every rendered value. Its optional Git revision fallback resolves Git to an
absolute executable, uses fixed arguments, discards stderr, caps stdout, and
has a two-second deadline. Installation, configuration, and state paths remain
visible because they are operational diagnostics; users should still review
the report before sharing it if local path names are sensitive.

`dahousekeeping` accepts migration roots and dependency floors only from the
embedding application. State migration is confined to an explicit list of direct
children, refuses symbolic links and special files, never overwrites a destination,
creates the state directory with owner-only permissions, and preflights and rolls
back the SQLite database/WAL/SHM group together. It is best effort: callers should
surface the path-free report and leave ambiguous copies for an operator rather than
deleting either one. The module-floor checker reads only caller-supplied Go build
metadata and never resolves packages or executes a repair command. Release binaries
already contain an immutable selected module graph; repository checks use `go mod
verify` and `go mod tidy -diff` instead of granting a running binary package-manager
authority.

Debug tracing is disabled unless explicitly enabled. The reusable handler bounds
record counts, message and attribute sizes, flattens control characters, and redacts
common credential-shaped attribute keys. Its file helper requires an absolute path
under an owner-private final directory, rejects linked or non-regular targets, forces
mode 0600 on Unix, admits only one process writer through an adjacent lock, and stops
before a finite byte limit. A process crash can leave the empty lock file behind; an
operator must verify no writer remains before removing that lock. Go does not expose
Windows ACL entries through `FileMode`, so the selected directory ACL remains the
Windows caller's responsibility. Redaction is defense in depth:
free-form messages and nonsensitive-looking attributes can still carry prompts,
responses, local paths, or secrets. Applications should log identifiers and protocol
state rather than payloads, keep trace paths private, and review traces before sharing.

`dainstall` is a closed-catalog process capability, not a general package-manager
wrapper. Catalog entries contain trusted, fixed executable and argument vectors;
caller input can only choose an exact normalized name. Process-backed entries require
an explicit authorization capability, run without a shell or standard input, discard
all subprocess output, use finite execution and lock waits, and serialize through an
application-private cross-process lock. The default executor passes only a small
operational environment allowlist rather than ambient API keys and other credentials.
On Unix it cancels the whole child process group; on Windows it directly terminates
the child and applies a finite wait. Catalog owners must review executable paths,
arguments, and third-party build code, set `LockPath` inside an owner-private
directory, and supply a stronger isolated executor when untrusted build code or child
process trees are in scope. `dacode install` currently identifies only integration
source packages already included in the release and refuses every `--package` name.
Applications must import and rebuild with those packages; the command never claims
that downloading a module can extend a running binary.

`daupdate` separates read-only checking, artifact-verifying dry runs, and executable
activation. Every channel manifest is a bounded versioned envelope carrying exact
payload bytes and an Ed25519 signature from the caller-supplied trust root. The signed
payload binds the channel, release version, artifact name, HTTPS URL, byte length,
SHA-256 digest, and expected Go package/module identity. Activation additionally reads
the staged binary's Go build metadata and rejects a different package, module, version,
or local module replacement. Checks and dry runs have no filesystem authority; apply
requires an exact authorization capability, a regular executable target, a finite
cross-process lock, and a same-filesystem verified temporary file before replacement.

The built-in HTTP source refuses redirects and cross-origin artifacts, bounds response
bodies, and the CLI disables ambient proxies. Its caller-owned transport still owns
DNS, certificate roots, and IP policy; an explicitly trusted release origin that
resolves to a private service can contact that service. The CLI ships no implicit
release origin or trust key, so missing provenance fails closed rather than trusting an
unsigned checksum beside an artifact. The CLI accepts its trust-root key only from an
absolute regular non-link file; Unix additionally requires current-user ownership and
rejects group/other write permission, while Windows relies on the file ACL. On Windows
the running executable cannot be
atomically replaced; check and dry-run remain available, while apply requires invoking
a separate trusted copy with `--target`. External writers are outside the update lock,
so operators should not concurrently replace the selected executable.

The terminal update surface derives no trust roots from environment, release metadata,
or the network. All channel, artifact, HTTPS manifest base, and public-key path inputs
must form one complete launch-time profile. `/update` checks before presenting a
two-step activation prompt, and `/auto-update` is preference-only when that profile is
absent. Owner-private atomic state records consent, exact skips, reminders, cooldowns,
and restart attempts; malformed, symbolic-link-backed, or unwritable state disables
automatic activation. Concurrent activation remains serialized by the updater's
cross-process lock. Notification URLs are fixed or validated credential-free HTTPS
addresses, copy actions only emit OSC 52 data, and failed preference writes retain the
action instead of claiming suppression. `/trace` builds its UI URL from a separately
trusted official web origin, refuses user information, fragments, controls, redirects,
and custom API-to-web origin inference, and never renders stored credentials or raw
provider errors.

Goal-criteria drafters and repository-backed rubric graders receive only `ls`,
`read_file`, `glob`, and `grep`. Absolute paths are lexically confined to the
configured repository root; traversal is rejected, reads and search results are
bounded, and every nested invocation has a finite shared call budget. No write,
delete, edit, or shell capability is derived. Backends remain responsible for their
own canonical path and symbolic-link confinement, so applications should give the
inspector a backend rooted at the repository rather than a broader host filesystem.
The rubric transport replay is limited to one retry and is safe only because callers
must supply read-only, idempotent supplemental grader tools.

Goal objectives, drafted criteria, rubric explanations, and per-criterion gaps are
untrusted model text. The terminal review and status surfaces render them through the
terminal-safe text boundary, and review edits are limited to 16,000 characters and
64 KiB. `/rubric file` accepts only a regular UTF-8 file and reads at most 1 MiB; the
selected backend and operating system remain responsible for resolving symbolic
links. Rubric graders can observe the bounded transcript and read-only repository
evidence described above, so criteria and transcripts must not be used as a secret
storage channel. A satisfied verdict is a model assessment rather than a sandbox or
authorization boundary; mutating tool approvals remain independently enforced.

Memory sources configured on an agent protect the machine-owned region delimited by
the managed onboarding markers. Filesystem edits are serialized across guarded paths;
malformed or duplicate markers, unreadable configured files, parent-directory deletes,
and path aliases fail closed. If a tool changes the managed region, the previous block
is restored while unrelated edits are retained when they can be merged safely; an
ambiguous oversized edit rolls the whole file back. The local filesystem restoration
path uses an atomic replacement, flushes the file and containing directory, preserves
the file mode, and stays inside the rooted filesystem boundary. The launch-policy
resolver consults only the bounded canonical Boolean policy and private marker, and it
suppresses modal onboarding for every non-interactive execution mode even when an
explicit launch request is present.

The coding agent exposes the selected agent's durable memory at
`/agent-memory/AGENTS.md` and its profile skills read-only below
`/agent-memory/skills/`. That routed mount is not mapped into the workspace shell
namespace, rejects other paths and writes outside the memory file, rejects
symbolic-link files and agent directories, and binds resumed threads to the agent
identity stored in their checkpoint. Personal memory is injected as fallible
reference material before project guidance; it does not override more-scoped
workspace instructions or direct user instructions.

Skill discovery treats every SKILL.md body as untrusted prompt input. Sources are
bounded and deterministic, and a later source can replace an earlier name, but that
does not grant the skill tool or network authority. A skill directory that resolves
outside its configured source is listed without reading its metadata or body and
requires approval of the exact canonical target before invocation. Grants are stored
in an owner-private, versioned JSON file through atomic replacement; linked, broad-mode,
oversized, duplicate, malformed, and unsupported stores fail closed. Each use resolves
the target again, so repointing a symbolic link cannot inherit approval. The TUI shows
the resolved local path because it is the object being trusted; users should avoid
sharing screenshots when path names are sensitive. The trust CLI never executes a
skill, and denial leaves the store unchanged.

Built-in skills are prompt instructions, not privileged code. `remember` requires an
explicit request before editing durable guidance, and `skill-creator` uses the same
ordinary file and approval boundaries as other work. The thread inspector receives a
required saver, reconstructs messages without executing pending work, applies finite
checkpoint/thread/message/text limits, and its CLI opens the caller-selected SQLite
database read-only. Transcript JSON can contain prompts, tool arguments, local paths,
and secrets; keep it local, request summaries when possible, and redact it before
sharing. The inspector does not repair, compact, or delete checkpoint data.

Named-agent discovery is bounded and requires a regular non-link `AGENTS.md` marker.
Hidden profiles, path-like or control-bearing names, and application-reserved `bin`,
`plugins`, and `conversation_history` names fail closed. Default and recent selections
are owner-private atomic settings; stale selections fall through without creating the
stale name. `agents reset` is deliberately destructive: it stages a private replacement,
refuses linked destinations, and offers `--dry-run`, but a completed reset removes the
destination profile's prior memory, skills, and session namespace. Copying a prompt
does not copy the source profile's skills or sessions. Reset rotates the namespace
and makes older checkpoints unavailable through normal run, resume, and listing
paths; it does not compact the shared checkpoint database, so removing sensitive
checkpoint bytes still requires deleting or maintaining that database separately.

The maintainer-wiki update workflow runs only on a trusted default-branch schedule or
manual dispatch, never on pull-request code. Actions and the OpenWiki package version
are pinned, job permissions are scoped to contents and pull requests, and the update
action can commit only `openwiki/` into a review pull request. The generator still runs
third-party package code with read access to the repository and an API credential in
the protected `openwiki` environment; environment approval, package-version review,
branch protection, and human review of generated prose remain required trust controls.
Wiki instructions prohibit secrets, machine paths, dirty-worktree content, and changes
outside documentation, but prompt instructions are not a sandbox.

Cost reports and local price catalogs are advisory data, not billing or authorization
boundaries. `dacost` bounds request ledgers, model rows, detail maps, catalog bytes and
entries, match recursion, regular expressions, strings, rates, per-request estimates,
persisted reports, and merged arithmetic. Invalid or future report versions fail
closed; failed observations and repricing attempts leave the prior totals unchanged.
Provider/model identifiers reject terminal controls, and cross-provider price matches
are disabled unless the embedding application opts in. Go regular expressions use
bounded input and RE2, avoiding backtracking denial of service.

Local override files are read as bounded regular files and never executed. Symlinks
are followed to their regular target, so applications that require a fixed trust root
must additionally confine the selected path. A missing or empty file means no local
rates; malformed and excessive files return an error that hosts should report without
failing a model turn. Catalog values and reports contain identifiers, token counts,
durations, and monetary estimates, which can reveal usage patterns; keep them private
when model names or activity are sensitive. Estimates must not gate approvals, enforce
budgets, or replace provider invoices.

Delegated and summarization usage is copied into the owning root checkpoint as bounded
normalized metadata. Reports reconstruct only from that root ownership chain and do
not enumerate unrelated checkpoint namespaces. A child response with malformed or
excessive usage metadata is omitted from accounting rather than failing the delegated
task. This favors availability but can undercount advisory totals; provider invoices
remain authoritative.

Standalone criteria drafting and automatic approval review likewise attach bounded,
cloned usage to the owning thread. Pre-checkpoint transfers are memory-only, capped at
64 thread identifiers and 256 records per thread, and are consumed only by an exact
thread match. Terminal reports cap model rows, truncate and sanitize external labels,
scope asynchronous results to the current thread generation, and replace local-price
load details with a generic notice. The exit summary is written only to the user's
terminal; reports and token counts should still be treated as private activity data.

The browser-only example stores its provider credential in `localStorage` and restores
it into Web Worker memory after reload. The credential, conversations, and
virtual-workspace files are readable to scripts on the deployment origin, so deploy it
on a dedicated origin and treat every same-origin script as trusted. The published
entry point includes a Content Security Policy, but that policy is defense in depth
rather than storage encryption or origin isolation.

Session resume is a trust boundary because a checkpoint may name a different agent
profile or an original working directory with different project instructions, skills,
environment files, hooks, and MCP configuration. The application-neutral resume
controller reads exact-thread metadata before transcript loading, validates bounded
thread, agent, and absolute-path values, verifies that a cross-agent target is still
available, and blocks loading until agent, directory, and high-context compaction
decisions are explicitly resolved. The controller pins and reloads the exact approved
checkpoint, rejecting any concurrent latest-checkpoint replacement. A canceled,
incomplete, or already-consumed controller cannot load a thread. Directory aliases are compared through canonical
symbolic-link resolution when both targets exist; unresolved aliases are treated as
different and require a prompt. The terminal application transactionally applies
the returned agent and directory transitions before calling the gated load helper.
If either transition or the exact load fails, it uses a separately bounded rollback
context to restore both prior values and does not expose the untrusted transcript.
Selected compaction runs only after the exact load succeeds. `/offload` and
`/compact` use the same server-owned path: the host executes the compiled compaction
tool against reconstructed exact state, preserves its private summary event, and
commits a child checkpoint only after successful summary generation. Provider
failure or cancellation leaves the approved checkpoint current.
The configured token threshold is bounded; zero disables the advisory compaction
prompt. Session paths, agent names, previews, and usage totals remain private local
activity data and must continue through terminal-safe rendering.

First-run onboarding accepts only bounded terminal-safe fields. Its completion
marker and optional preferred-name memory update use owner-only directories/files
and durable replacement; unsafe, oversized, symbolic-link, or malformed markers
fail closed by offering setup again. The model selection callback receives only a
validated catalog value. Restart authority is narrower still: `/restart` exists only
for a server process launched and owned by the current terminal runtime, shows a
confirmation modal, inherits no new credentials, and uses the supervisor's bounded
stop/start, loopback-readiness, log-redaction, and process-tree cleanup contract.

The interactive credential manager never renders entered API keys, OAuth session
documents, or raw dependency errors. Background reads return immutable
generation-tagged snapshots and only the terminal update loop mutates visible state,
preventing stale refreshes from restoring removed credentials. Secret buffers and
subscription flows are cleared or cancelled on every modal exit and terminal shutdown;
late flow events are ignored. Changes use the same owner-private atomic stores and are
applied only when a later runtime is explicitly constructed.

The interactive MCP viewer exposes bounded terminal-safe names, tool summaries, and
fixed error classifications, not resolved headers or tokens. Authorization URLs must
be credential-free HTTPS and are displayed without query data; full URLs are available
only through the explicit clipboard action. Callback and workspace responses are
bounded and hidden. Session-disable mutations serialize with reconnect, and pending
state is cleared only by the matching successful rebuild, so concurrent toggles cannot
silently re-enable a server. Ctrl+C, Ctrl+D, Esc, timeout, and shutdown all cancel or
close their active modal without retaining secret input.

Auto review is a separate model-disclosure boundary. It receives only bounded,
control-free trusted-user rows, safe interactive-question result summaries, redacted
and recursively summarized action arguments, bounded metadata, and exact validated
tool-call IDs. Assistant text and raw tool results are excluded. Header values,
credential assignments and URLs, known argument secrets, and private-key material are
redacted before construction, and the final prompt has a strict UTF-8-safe size cap.
Malformed batches, missing IDs, provider or configuration failures, unavailable state,
and persistence failures fail closed; cancellation remains distinguishable and does
not consume or latch classifier state.
