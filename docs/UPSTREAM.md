# Upstream manifest

This manifest anchors every normative source used by the port. Updating a revision
requires reviewing the named surfaces, their tests, generated compatibility fixtures,
and any affected intentional differences.

| Project | Repository | Revision | Local checkout |
|---|---|---|---|
| Deep Agents Python | `langchain-ai/deepagents` | `d60560d695e8c436e11dee96965e7a1447409737` | `/Users/ramon/src/deepagents` |
| LangGraph Python | `langchain-ai/langgraph` | `fde3068970679184b68d3d068a92c83c966a4888` | `/Users/ramon/src/langgraph` |
| LangChain Python | `langchain-ai/langchain` | `d048fbe170573b6e7056b5ef5f78d8451e54abaf` | `/Users/ramon/src/langchain-py` |
| Deep Agents TypeScript | `langchain-ai/deepagentsjs` | `945b362d06d03728d16bc0020cb242a9eeae8451` | `/Users/ramon/src/deepagentsjs` |
| LangChain TypeScript | `langchain-ai/langchainjs` | `62fc484b2a0d1ec5b8bebff4a8a0efe6300ada72` | `/Users/ramon/src/langchain` |

## Normative surfaces

### Deep Agents

- `libs/deepagents/deepagents/graph.py`
- `libs/deepagents/deepagents/backends`
- `libs/deepagents/deepagents/middleware`
- Public tests for constructor, middleware, backends, subagents, context management,
  skills, memory, profiles, and structured responses

### LangGraph

- `libs/langgraph/langgraph/channels`
- `libs/langgraph/langgraph/graph`
- `libs/langgraph/langgraph/pregel`
- `libs/langgraph/langgraph/types.py`
- `libs/checkpoint/langgraph/checkpoint`
- `libs/checkpoint-sqlite/langgraph/checkpoint/sqlite`
- `libs/checkpoint-postgres/langgraph/checkpoint/postgres`
- `libs/checkpoint-conformance/langgraph/checkpoint/conformance/spec`

### LangChain

- `libs/langchain_v1/langchain/agents/factory.py`
- `libs/langchain_v1/langchain/agents/middleware`
- `libs/langchain_v1/langchain/agents/structured_output.py`
- `libs/langchain_v1/langchain/chat_models/base.py`
- `libs/core/langchain_core/messages`
- `libs/core/langchain_core/language_models`
- `libs/core/langchain_core/tools`
- `libs/core/langchain_core/runnables/config.py`

## Provenance rules

- Port observable contracts and intended behavior, not source structure.
- Cite the upstream file and test in every generated compatibility fixture.
- Preserve upstream license notices when code or fixture data is copied
  substantially.
- Treat existing Go experiments as non-normative until a behavior is independently
  established by the pinned sources.
