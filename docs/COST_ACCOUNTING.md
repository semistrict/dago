# Cost and token accounting

`dacost` is the reusable accounting boundary for applications that need
per-session token and estimated-cost reports. It is UI-neutral: it does not
start network requests, mutate checkpoints, limit spend, or grant model
authority.

## Recording a request

Create one `Tracker` per loaded session and call `Record` with the stable model
request ID. A completed response is recorded once. For incremental streams,
set `Observation.Incremental`; later observations with the same ID revise the
same request rather than creating new rows. Call `Finalize` at every stream
round boundary before retaining the tracker across an interrupt or resume.

An empty request ID is accepted for providers that do not identify responses,
but each such observation is necessarily a separate request. The bounded ledger
eventually returns `ErrLimitExceeded` rather than silently evicting replay
protection.

Response-reported `Usage.Model` and `Usage.Provider` take precedence over the
configured fallbacks. This lets a final zero-token chunk move all earlier usage
from a fallback model to the actual model and reprice it. Incremental negative
token corrections are merged before display counts are clamped at zero.

Requests are grouped by the exact provider/model pair and by one of Assistant,
Subagent, Offload, or Auto. Cache reads and generic, five-minute, and one-hour
writes are tracked separately and clamped to the inclusive input total. The
normalizer accepts both inclusive provider input and the repository's existing
uncached-input-plus-cache-details representation.

`Report` returns versioned JSON-safe arrays in deterministic provider/model and
purpose order. `DecodeReport` and `MergeReports` enforce byte, row, arithmetic,
version, coverage, and non-finite-number checks before restored turn reports are
combined into a session report.

`ReportMessages` can rebuild the same stable report from checkpointed messages.
Top-level response usage is Assistant work. `TransferUsage` flattens a completed
nested run into the parent task result as Subagent usage, while preserving nested
Offload and Auto labels. Summarization usage is likewise attached to its durable
summary event. These ownership transfers make root-session reconstruction survive
restart without scanning or trusting independently retained child namespaces.

`NormalizeUsage` is a transparent `damodel.Chat` decorator used at the runner's
central model-resolution boundary. It preserves optional tool-binding and token-
counting capabilities, supplies configured provider/model names when a response
omits them, and turns signed streaming fragments into cumulative usage. Late
model names therefore refile the completed request correctly, including runtime
model changes and custom subagent or rubric models.

## Pricing catalogs

`NewPricer` takes primary, bundled-stopgap, and local catalogs positionally.
Lookup order is primary, local, then bundled, so a local entry cannot replace a
known primary rate. Provider aliases are normalized before lookup. A model match
under another explicitly named provider is rejected unless
`AllowCrossProviderMatch` is deliberately enabled.

`LoadCatalog` reads a bounded regular JSON file using the provider-array schema:

```json
[
  {
    "id": "my-proxy",
    "name": "My proxy",
    "api_pattern": "gateway\\.example\\.internal",
    "models": [
      {
        "id": "house-model-v2",
        "match": {"equals": "house-model-v2"},
        "prices": {
          "input_mtok": 2.5,
          "output_mtok": 10.0,
          "cache_read_mtok": 0.25,
          "cache_write_mtok": 3.0
        }
      }
    ]
  }
]
```

Rates are USD per million tokens. Model matching supports `equals`,
`starts_with`, `contains`, `regex`, and bounded `or` alternatives. Missing or
empty local files are empty catalogs. Invalid JSON, duplicate identifiers,
invalid regular expressions, excessive nesting/counts/strings, and negative,
non-finite, or excessive rates fail explicitly. Applications should load the
file once when building the session pricer; edits then take effect on the next
session.

The bundled catalog is available through `BundledCatalog`. It is a small,
maintainer-curated stopgap rather than a comprehensive or authoritative rate
source. The interactive runner loads `prices.json` once from its private state
directory, applies it before the bundled stopgaps, and exposes any rejected
local-catalog error separately from the usable report. A malformed override
therefore cannot prevent a model turn and cannot partially install rates.

A nil estimator still records tokens and request coverage. A known literal-zero
price counts as priced, while unavailable pricing increments the unpriced request
count. `Reprice` atomically swaps estimators and recomputes catalog-derived
requests without replacing explicit provider-reported costs.

## Host integration boundary

The interactive runner exposes `CostReport` as an optional capability without
widening the base runner contract. It reconstructs top-level, delegated, and
summarization usage from the root checkpoint and uses response model/provider
names before configured fallbacks. Every model resolved by the runner is usage-
normalized. Standalone automatic-review and criteria-drafting calls transfer their
bounded usage into the owning thread as Auto and Assistant work, respectively.
If a criteria call precedes the first checkpoint, the runner holds a cloned,
bounded pending transfer for at most 64 threads and attaches it to that thread's
next input.

The terminal status line shows the current estimated session cost. `/cost` renders
bounded provider/model and purpose rows, cache reads and writes, unpriced request
coverage, and total model time; `/tokens` includes the same session totals alongside
the current context window. A final private usage summary is printed after the
alternate screen closes. Loading is asynchronous and generation-scoped so a late
report cannot overwrite a newly loaded thread, and local-pricing errors are generic
while the usable report remains visible. Browser-terminal coverage verifies the
detailed report and exit summary. Estimated costs are display-only and must never
substitute for provider billing or approval policy.
