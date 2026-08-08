# Support policy

## Go versions

Dago requires Go 1.26 or newer. The normal test suite targets the current Go 1.26
release on Linux and macOS. Support for another operating system is not claimed until
its filesystem, process, cancellation, and database conformance suites are running in
continuous integration.

## Compatibility claims

Compatibility is behavioral and limited to the subset documented in `TODO.md` and
the generated compatibility matrix. The project does not claim general LangChain or
LangGraph compatibility.

SQLite and PostgreSQL saver schemas and operations target the pinned Python sources.
Cross-language payload compatibility is limited to the documented safe plain-data
codec. Python-specific object reconstruction, pickle, dynamic imports, and serialized
callables are unsupported.

## Stability

The public API is unstable until the first tagged release. Internal packages may
change without notice. Persistent data formats must remain readable once they appear
in a tagged release unless release notes describe a migration.
