# Deterministic behavioral evaluations

The `daeval` package turns an agent result into a provider-neutral trajectory and
evaluates intended behavior without network access, credentials, tracing, or a
hosted reporting service. Normal tests should use `damodel/modeltest` scripts so
the model actions, tool observations, file snapshot, and final answer are all
repeatable.

The package follows the behavioral harness in Deep Agents revision
`217b9eb372fa51b0439434f31abc3ac22e6cd7f2`, principally:

- `libs/evals/tests/evals/utils.py` for trajectory construction, correctness
  checks, and non-gating efficiency expectations;
- `libs/evals/tests/evals/test_tool_selection.py` and
  `test_file_operations.py` for representative intended-behavior cases; and
- `libs/evals/tests/evals/pytest_reporter.py` for aggregate correctness,
  per-category scores, bounded failure details, and micro efficiency ratios.

## Writing an evaluation

Required runtime dependencies are positional. Construct a case with
`daeval.NewEvaluation(run)` and use `daeval.Invoke(agent, options...)` for a compiled
agent:

```go
evaluation := daeval.NewEvaluation(daeval.Invoke(agent, dagent.Prompt("Create the note")))
evaluation.Name = "creates requested note"
evaluation.Category = "files"
evaluation.Correctness = []daeval.Check{
    daeval.ToolCalled("write_file").WithArguments(map[string]any{
        "file_path": "/note.md",
    }),
    daeval.FinalTextContainsFold("created"),
}
evaluation.Expectations = []daeval.Check{
    daeval.StepCount(2),
    daeval.ToolCallCount(1),
}

report := (daeval.Harness{}).Evaluate(ctx, evaluation)
```

Correctness checks determine pass or fail. Expectations are deliberately soft:
an inefficient but correct trajectory remains passed, while unmet expectations
and aggregate step/tool-call ratios remain visible in the report. Put a check in
`Correctness` when violating it is wrong behavior, including forbidden tool calls
or unauthorized file mutation.

`Harness` has useful secure defaults: cases run sequentially in declaration
order, individual failure details are bounded to 30,000 bytes, category names
default to `uncategorized`, and missing labels receive stable sequence names.
The version-1 report contains no current time or wall-clock duration, so marshaled
reports from identical trajectories are byte-for-byte stable. Execution errors
and cancellation are reported separately from behavioral failures and are
excluded from correctness denominators.

Custom checks implement the small `daeval.Check` interface. Check factories put
mandatory names, paths, expected content, and counts in positional parameters;
invalid static selectors panic during construction rather than failing later in
a run.

## Upstream differences

The upstream live tier invokes paid models, publishes tracing feedback and
experiment links, and records duration-derived solve rate. Those operations are
environmental rather than agent behavior, so they are intentionally outside the
default Go harness. Callers may perform them around a `daeval.Run`; they do not
change trajectory or scoring semantics.

Go uses one context-aware execution path rather than separate synchronous and
asynchronous helpers. Reports distinguish runtime errors from incorrect answers
instead of rewriting an evaluation-process exit status. The separate unified
cross-model workflow remains its own compatibility item.

## Sandboxed Harbor benchmarks

The `daeval/harbor` subpackage ports the local, provider-neutral contracts from
`libs/evals/deepagents_harbor`. An application supplies the required sandbox runner
positionally; the package itself has no provider SDK, registry, subprocess, network,
credential, or hosted-tracing dependency:

```go
runner := harbor.RunnerFunc(func(ctx context.Context, task harbor.Task) (harbor.Trial, error) {
    // Invoke the application's already-authenticated, isolated sandbox transport.
    return runSandboxTrial(ctx, task)
})

task := harbor.NewTask("repair-parser", "Fix the parser and run its tests")
task.Category = "terminal"
task.Correctness = []daeval.Check{daeval.FileContains("/parser.go", "Parse")}

report := harbor.NewBenchmark(runner).Evaluate(ctx, task)
```

The verifier reward is a hard correctness check with a useful default minimum of
`1`. Missing rewards become zero instead of silently passing. Caller checks remain
additional correctness and efficiency assertions in the nested `daeval.Report`.
Infrastructure failures are separated from capability failures using exit codes and
controlled exception text. Exit codes are extracted only from tool observations, so
an assistant discussing “exit code 137” cannot make its own wrong answer disappear
from the score denominator.

The default harness runs at most 1,000 ordered tasks, allows ten minutes per trial
and one hour overall, and bounds task text, metadata, checks, trajectory steps, tool
calls, observations, structural items, result bytes, and failure details. Adjust
those exported limits before evaluation when a benchmark legitimately needs more.
The supplied runner must honor context cancellation; the harness cannot forcibly
stop an opaque remote transport. Reports omit timestamps and durations, preserve
declaration order, expose tasks omitted by the work bound, and include a 95% Wilson
score interval plus a conservative two-run minimum detectable effect estimate.

`harbor.ExampleID` reproduces the pinned instruction-derived ID with seed 42.
`ClassifyFailure`, `ExtractExitCodes`, `WilsonInterval`, and
`MinimumDetectableEffect` are also public with useful 95% defaults for importers
that already own a separate execution workflow.

Hosted dataset creation, experiment sessions, and feedback publication are not part
of this package. Those operations require service-specific credentials and network
policy and remain explicit application integrations. The unified cross-model scorecard
is tracked separately.

### ContextBench and DRBench records

The Harbor package adapts already-acquired ContextBench and DRBench records without
reading a checkout or contacting a registry. Required task IDs, questions, and the
DRBench application manifest are positional constructor inputs; optional labels and
agent-visible company/persona fields have useful zero-value fallbacks:

```go
contextRecord := harbor.NewContextBenchRecord("cb-cloud-17", question)
contextRecord.Difficulty = "easy"
contextTask, err := contextRecord.Adapt()

researchRecord := harbor.NewDRBenchRecord(
    "DR0001",
    researchQuestion,
    harbor.DRBenchNextcloud,
    harbor.DRBenchEmail,
)
researchRecord.Company = harbor.DRBenchCompany{Name: "Acme", Industry: "Retail"}
researchRecord.Persona = harbor.DRBenchPersona{Name: "Dana Ray", Role: "Compliance Lead"}
researchTask, err := researchRecord.Adapt()
```

ContextBench produces the pinned `/app/files` prompt, `/app/answer.txt` output
contract, `context` category, source labels, and allowlist policy metadata. It does
not accept ground truth: the answer and model-judge rubric stay in the caller's
verifier environment.

DRBench produces the pinned deep-research prompt and `research` category. It sorts
and de-duplicates the selected application manifest, describes only those endpoints,
uses safe public usernames and environment-variable names for passwords, requires the
report at `/app/report.md`, and preserves the exact email/chat citation forms that its
judge resolves. Gold insights, distractor answers, verifier prompts, and passwords
have no fields in `DRBenchRecord`; the pinned task-config revision is recorded only as
provenance metadata.

`AdaptContextBench` and `AdaptDRBench` preserve declaration order, check cancellation
between records, and reject batches above 1,000. Single-record adaptation bounds UTF-8
text, profile lists, rendered prompts, application names, usernames, and non-negative
metadata counts. Dataset downloads, corpus staging, image-digest resolution, sandbox
environment variables, egress enforcement, and verifier execution belong to the
caller-supplied runner. Adapter network-policy metadata is descriptive and grants no
authority by itself.

## Unified cross-model scorecards

The `daeval/scorecard` package runs one identical ordered Harbor task matrix across
multiple models. A required runner resolves provider clients, credentials, model
options, and sandbox execution outside the library. Model identities are deliberately
non-secret labels:

```go
runner := scorecard.RunnerFunc(func(
    ctx context.Context,
    model scorecard.Model,
    task harbor.Task,
) (harbor.Trial, error) {
    return runWithConfiguredModel(ctx, model.ID, task)
})

strong := scorecard.NewModel("provider-a:strong")
strong.Provider = "provider-a"
candidate := scorecard.NewModel("provider-b:candidate")

report := scorecard.New(runner, strong, candidate).Evaluate(
    ctx,
    contextTask,
    researchTask,
)
```

The runner and first model are positional required inputs, constructors do not return
errors, and invalid or duplicate static identities panic at construction. Provider
credentials and configuration are intentionally absent from `scorecard.Model`; a
runner should resolve them out of band by the stable ID.

Evaluation is model-major and sequential so call order and reports are stable. Each
model receives the same tasks through the Harbor harness, including reward checks,
failure attribution, payload limits, cancellation, and confidence statistics. The
version-1 scorecard contains complete per-model Harbor reports, aggregate correctness
with a 95% Wilson interval and conservative minimum detectable effect, category rows
sorted by name, per-category model results in declaration order, and a deterministic
leaderboard. It records no wall-clock time, duration, hosted run ID, or credential.

Zero-valued configuration uses limits of 16 models, 1,000 tasks, and 10,000 total
model-task runs, with four hours overall, one hour per model, and ten minutes per
trial. Oversized matrices fail before the first runner call rather than truncating one
model's task set and producing an unfair comparison. Negative limits and non-finite
reward thresholds panic as invalid static configuration. A runner must still honor
context cancellation; an opaque transport that ignores its context cannot be forcibly
terminated by the scorecard.

The statistics assume each scored model-task outcome is a binomial observation. Real
benchmark tasks and repeated model samples may be correlated, and changing a verifier
or judge invalidates direct score comparisons. Rankings are reporting aids, not claims
of statistical significance or production fitness. Hosted publication, artifact
storage, billing, retries, and cross-run baselines remain application workflows.

## Continual-learning benchmark systems

The `daeval/clbench` package ports the pinned `deepagents_clbench` system lifecycle
without importing a benchmark runtime or model provider. Applications supply the
required schema-aware factory positionally; it owns structured-agent construction,
provider authentication, and any optional tools:

```go
factory := clbench.FactoryFunc(func(ctx context.Context, config clbench.AgentConfig) (clbench.Agent, error) {
    return buildStructuredAgent(ctx, config.ResponseSchema, config.SystemPrompt, config.MemorySources)
})

system := clbench.New(factory, clbench.Options{})
query := clbench.NewQuery("Choose the next action", clbench.NewSchema("action", actionSchema))
response, err := system.Respond(ctx, query)
```

Each response performs exactly one agent interaction. The factory is called once per
canonical named JSON schema and receives the fixed continual-learning prompt plus
`/memory/AGENTS.md` as its only memory source. The agent receives an owned copy of the
in-state filesystem; a validated returned filesystem is threaded to the next turn.
`Observe` retains nonblank outcome text without another model call, and the next query's
own nonblank feedback takes precedence. `Reset` restores the empty strategy scaffold,
clears pending feedback and the interaction count, and deliberately retains only the
schema-agent cache and run-level usage accounting. This makes a reset-before-each-case
baseline stateless while avoiding unnecessary agent reconstruction.

Responses expose the native structured JSON action, stable model/system/interaction
metadata, and the memory-file snapshot expected by benchmark viewers. Run artifacts
contain the same memory snapshot. Per-message usage records aggregate into one
completion event per turn, matching the pinned adapter's accounting shape.

Zero options select finite defaults: ten minutes per turn, 10,000 interactions, 128
distinct schemas, and bounded prompt, schema, action, file, aggregate-state, usage,
token, label, and error sizes. Factory and agent panics become generic errors; wrapped
operational errors retain `errors.Is`; caller cancellation wins even if an implementation
returns a result afterward. Files must use clean absolute virtual paths and UTF-8 or
valid base64 content, and invalid output cannot partially update retained memory.

Unlike the Python deployment payload, Go does not copy source into a benchmark checkout
or import its registry/interface modules. Registration, schedule selection, rollout and
baseline orchestration, task-specific schema validation beyond the agent's structured
output contract, pricing, and result storage remain with the benchmark application.
