# Plasmid - Engineering Standards

Plasmid is a CLI-free, in-process coding-agent harness for Go built directly on
Google ADK v2. It embeds in a host program, reads compatible skills and plugins
already installed by Claude Code, Codex, and Copilot, and injects LSP
diagnostics into successful write and edit tool results.

These rules bind every contribution.

## Product boundary

- V1 is Go-only. Data fixtures stay implementation-neutral where practical,
  but hypothetical ports never dictate Go APIs.
- The harness has no TUI, CLI, RPC mode, subprocess SDK, subagents, plan mode,
  permission-popup UI, built-in todos, or background bash.
- Plasmid installs no skills, plugins, MCP servers, or LSP servers. It resolves
  existing inputs and activates only behavior authorized by the host.

## Toolchain and dependencies

- Use Go 1.26.6 or newer for development, CI, and release builds. Both module
  directives remain at ADK v2.2.0's Go 1.26.5 language floor. Keep `gofmt`,
  `go vet`, and module verification clean.
- Approved direct modules are `google.golang.org/adk/v2`,
  `google.golang.org/genai`, `github.com/google/jsonschema-go`,
  `github.com/modelcontextprotocol/go-sdk`, `go.lsp.dev/protocol`, and
  `github.com/sourcegraph/jsonrpc2`. Use only the modules needed by the code.
- Any other dependency requires a PR justification covering why the standard
  library is insufficient, an OSI license, latest stable pin, active
  maintenance, and known-CVE review.
- Implement repository walking, matching, parsing, and file operations in Go.
  Do not shell out to host utilities that may be absent. `git` is the sole
  exception, limited to repository-root discovery.

## Architecture

- Use Google ADK directly. The root `Harness` owns native `llmagent` and
  `runner` construction, active-run cancellation, and resource lifecycle.
- Packages that implement an ADK extension point may import native ADK types:
  the root harness, coding tools, durable session service, callbacks,
  compaction, skill and MCP toolsets, and compiled-plugin integration.
- Keep leaf concerns framework-free: workspace containment and touches, output
  limiting, shell execution, path matching, parsing, normalization, foreign
  scanning, and fixture mechanics. Do not add a provider-neutral agent-loop
  abstraction. A package-boundary test enforces the explicit native-ADK
  integration allowlist.
- Coding tools are native ADK function tools. `sessionstore` implements native
  ADK `session.Service`. Public run streams use native ADK session events.
- One package owns each shared concern. Glob matching, frontmatter parsing,
  Markdown code-region scanning, warning records, truncation markers, config
  validation, touch events, and fixture comparison each have one source of
  truth.
- Use descriptive package, type, function, and wire names. The Plasmid metaphor
  remains branding.
- Resolution and discovery fail soft per malformed entry and emit one stable,
  structured warning. Construction and invariant violations fail explicitly.

## Security

- Discovery performs bounded, cancellation-aware file I/O only. It never runs
  hooks, scripts, lifecycle code, prompt commands, MCP servers, or LSP servers.
- Repository-scoped extensions require host trust before automatic model
  exposure. Foreign permission fields are metadata and never grant Plasmid
  authority.
- Foreign MCP declarations remain inert until an exact Plasmid allowlist entry
  or explicit inherit-foreign consent enables them. Wildcards do not grant
  access. Warnings and fixtures never expose secrets.
- File tools resolve every path beneath configured workspace roots. This is a
  correctness boundary.
- Bash and prompt-expansion commands use the single bounded executor with cwd,
  timeout, cancellation, process cleanup, and output limits. They retain the
  full authority and inherited environment of the host process. Hosts needing
  isolation must confine that process.
- Prompt command execution uses `off`, `trusted`, or `on`, default `trusted`.
  Foreign content cannot execute commands in `trusted` mode.
- LSP servers are detected and started lazily, never installed. Failure or
  timeout degrades to no-op with one warning and cannot exceed configured
  bounds.

## Testing and completion

- Every behavioral rule lands with a table-driven test or deterministic fixture
  under `testdata/fixtures/<area>/<case-id>/`.
- `internal/fixture` is the sole fixture loader and comparator. Warning fixtures
  assert stable codes and structured fields, not prose.
- ADK seams use offline full-turn tests with a fake `model.LLM`, real
  `llmagent`/`runner`, native tools, and the durable session service. Tests never
  require network access.
- Run targeted race tests for shared mutable state and Windows compilation for
  filesystem, path, process, and LSP changes.
- Progress states are `missing`, `source present`, `targeted verified`,
  `integrated`, and `frozen`. Only `frozen` is complete. A receipt advances only
  with current executable evidence recorded in `PLAN.md`.
- Before commit, run the cheapest sufficient targeted checks plus
  `go build ./...` and `go vet ./...`. Release gates also include all tests,
  race coverage, fixture verification, module verification, vulnerability
  scanning, and required platform builds.

## Documentation

- The repository carries code documentation only: README, godoc comments,
  `AGENTS.md`, and user-facing docs for released behavior.
- Update README in the same PR as any released user-facing behavior. Planned
  behavior belongs in ignored local `SPEC.html` and `PLAN.md`, not README.
- Plans, generated specs, research, and discussion notes remain ignored local
  artifacts (`PLAN.md`, `SPEC.html`, `notes/`, `docs/design/`, `*.notes.md`). Do
  not weaken those ignore rules or commit those files.
- Comments state constraints the code cannot express. They do not narrate code
  or cite conversations and plans.

## Commits and pull requests

- License: MIT.
- Use signed conventional commits in imperative mood with plain ASCII. Do not
  add AI attribution, generated-by footers, session links, or non-human
  co-author trailers.
- `main` is protected. Use pull requests, linear history, squash merge, and
  automatic head-branch deletion. Do not force-push or create merge commits on
  `main`.
- Every commit compiles and passes its relevant tests. Commit and publish only
  task-scoped files; local design artifacts stay local.
