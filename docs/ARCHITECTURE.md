# Architecture and public API policy

dago is a focused Deep Agents implementation, not a general port of LangChain or
LangGraph. The dependency direction is:

`damessage/damodel/datool/dastate` → `dastore/dacache/dacheckpoint` → internal graph
→ generic agent → deep middleware and backends → provider and remote adapters →
examples.

Core contracts contain no provider SDK types. Database drivers live in concrete
subpackages. The graph runtime remains internal until it has a separately reviewed
public use case; public consumers use `dagent.Agent` or `dago.DeepAgent`.

Public additions require intended-behavior tests, cancellation semantics for any
blocking operation, a stable JSON representation for persisted or streamed data,
and a documented compatibility row. Errors wrap stable sentinel errors where a
caller can reasonably recover. Secret values are never included in error text.

Persistent envelopes and stream records begin at version 1. Additive fields are
allowed within a major module version. Removing or reinterpreting a field requires a
new envelope version and a migration. Unknown versions fail explicitly.

The test suite distinguishes normative tests, which assert behavior anchored in the
upstream manifest, from exploratory reproductions. Reproductions are not kept in the
passing suite unless their assertion has been flipped to the intended behavior.
