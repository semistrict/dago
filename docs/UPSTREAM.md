# Upstream manifest

[`upstream-manifest.json`](upstream-manifest.json) pins every normative source
used by the port. Updating a revision requires reviewing the named source
surfaces, their tests, generated compatibility fixtures, and intentional
differences.

Optional local checkouts are configured by environment variable; no checkout
path is embedded in the repository.

| Project | Revision | Checkout variable |
|---|---|---|
| Deep Agents Python | `d60560d695e8c436e11dee96965e7a1447409737` | `DEEPAGENTS_PYTHON_ROOT` |
| LangGraph Python | `fde3068970679184b68d3d068a92c83c966a4888` | `LANGGRAPH_PYTHON_ROOT` |
| LangChain Python | `d048fbe170573b6e7056b5ef5f78d8451e54abaf` | `LANGCHAIN_PYTHON_ROOT` |
| Deep Agents TypeScript | `945b362d06d03728d16bc0020cb242a9eeae8451` | `DEEPAGENTS_TYPESCRIPT_ROOT` |
| LangChain TypeScript | `62fc484b2a0d1ec5b8bebff4a8a0efe6300ada72` | `LANGCHAIN_TYPESCRIPT_ROOT` |
| Shelley | `1d4cbe79c6be45cc0105d46819cb54844f98eddd` | `SHELLEY_UPSTREAM_ROOT` |

`make drift` validates the manifest structure and verifies the exact Git
revision of each checkout whose variable is set. Missing optional checkouts do
not make the normal suite machine-specific.

## Provenance rules

- Port observable contracts and intended behavior, not source structure.
- Cite the upstream project, repository-relative file, and test selector in
  generated compatibility fixtures. When the matching pinned checkout is
  configured, generation validates all three against that exact revision.
- Preserve license notices when source or fixture data is copied substantially.
- Treat experiments as non-normative until pinned sources establish a behavior.
