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
| Deep Agents QuickJS | `217b9eb372fa51b0439434f31abc3ac22e6cd7f2` | `DEEPAGENTS_QUICKJS_ROOT` |
| quickjs-rs | `278cf32d17b07a9ba2951ebc826256eef703182d` | `QUICKJS_RS_ROOT` |
| Agent Client Protocol | `70286d45bcea5cdc0afd7b0f14a80488ccded2e9` | `ACP_ROOT` |
| ACP Go SDK | `0845a3bb9eddda5bfc22a94dd3598c90cb842451` | `ACP_GO_SDK_ROOT` |
| WAFL | `c1585f4c3efbf2ba9354d1989cee8f075d013f27` | `WAFL_ROOT` |

`make drift` validates the manifest structure and verifies the exact Git
revision of each checkout whose variable is set. Missing optional checkouts do
not make the normal suite machine-specific.

The Deep Agents QuickJS suite-by-suite coverage and intentional Go runtime
boundaries are recorded in [`QUICKJS_TEST_PORT.md`](QUICKJS_TEST_PORT.md).

## Provenance rules

- Port observable contracts and intended behavior, not source structure.
- Cite the upstream project, repository-relative file, and test selector in
  generated compatibility fixtures. When the matching pinned checkout is
  configured, generation validates all three against that exact revision.
- Preserve license notices when source or fixture data is copied substantially.
- Treat experiments as non-normative until pinned sources establish a behavior.
