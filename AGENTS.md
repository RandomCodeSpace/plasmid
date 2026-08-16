# Plasmid — Engineering Standards

Plasmid is a CLI-free, in-process coding-agent harness for Go, built on Google
ADK (google.golang.org/adk/v2). It embeds in a host program (no TUI, no CLI, no
subprocess SDK), reads skills and plugins already installed by Claude Code,
Codex, and Copilot, and enforces LSP diagnostics into the agent loop.

These standards are binding for all contributions, human or agent.

## Toolchain and dependencies

- Go 1.24+ (CI pins the exact version). `gofmt` and `go vet` clean at all times.
- Allowed dependencies: `google.golang.org/adk/v2`, `go.lsp.dev/protocol`,
  `github.com/sourcegraph/jsonrpc2`, and the standard library. Nothing else
  without a written justification in the PR description: OSI license
  (MIT/Apache/BSD), latest stable, pinned, active maintenance, no CVEs.
- Never shell out to tools that may not exist on the host (grep, ripgrep, git
  assumed present is the one exception, and only for repo-root discovery).
  Pure-Go implementations only.

## Architecture rules

- The loop-provider boundary is law: the normative core (everything outside
  the ADK adapter package) never imports ADK, genai, or any framework type.
  Core packages use the framework-free `loop.*` contract types only; the ADK
  adapter is the single package that touches ADK. A CI test enforces this on
  every top-level package, not a subset.
- Minimal core: no subagents, no permission popups, no plan mode, no built-in
  todos, no background bash, no TUI/CLI/RPC. New capability arrives as a
  skill, a plugin, or an MCP server — never as a built-in. If a feature can be
  an extension, it is one.
- Descriptive names only. Packages, types, functions, and wire-format names
  say what they do (`tools`, `sessionstore`, `lsp`, `syntax`). The plasmid
  metaphor lives in branding, never in identifiers.
- One implementation per concern. Shared machinery (glob matching, frontmatter
  parsing, truncation markers, warning types, touch events) has exactly one
  owner package; a second parser or a second marker string for the same
  concern is a defect.
- Fail soft, warn loud: resolution and discovery never crash on malformed or
  unknown input — skip it and emit one warning line through the shared warning
  type. Silent degradation is a defect; so is a fatal on foreign data.

## Security stances (non-negotiable)

- Nothing executes at resolution/discovery time. Reading config, plugins,
  skills, and instruction files is file I/O only. No install hooks exist.
- Foreign MCP declarations load inert; they activate only via the config
  allowlist or explicit consent flag.
- Prompt-expansion command execution (the `!` syntax) runs only through the
  single sandboxed bash executor (timeouts, output caps, cwd discipline) and
  honors the global disable flag.
- LSP servers are detected, never installed. A crashed or hanging server
  degrades to no-op; it never blocks tool execution.
- File tools never resolve paths outside the sandbox roots. The sandbox is a
  correctness boundary for file tools; bash is documented as not confined by
  it — hosts wanting isolation confine the process.

## Testing and conformance

- Every behavioral rule lands with a table-driven test. Golden/fixture tests
  use the repository's single fixture layout convention; fixtures are written
  language-neutral (they seed the future conformance suite for the Python,
  Java, and TypeScript implementations).
- A fake `model.LLM` / loop provider drives full-turn tests; no network in
  tests, ever.
- The cheapest sufficient check runs before commit: `go build ./...`,
  `go vet ./...`, targeted tests for the touched packages.

## Documentation policy

- The repository carries code documentation only: README, godoc comments,
  AGENTS.md, and user-facing docs for released behavior.
- Design documents, implementation plans, discussion notes, meeting notes,
  brainstorms, and generated specs are never committed or pushed. They live
  locally and are covered by .gitignore (PLAN.md, SPEC.html, notes/, docs/design/,
  *.notes.md). Do not weaken these ignore rules.
- Code comments state constraints the code cannot express — never narration,
  attribution, or references to conversations or plans.

## Commits and PRs

- License: MIT. All contributions are accepted under it.
- Conventional commit messages (`feat:`, `fix:`, `refactor:`, `test:`,
  `chore:`), imperative mood, plain ASCII.
- Every commit is signed. Unsigned commits are rejected by branch protection;
  configure SSH or GPG signing before contributing.
- No co-authors by default: no Co-Authored-By trailers unless a second human
  actually co-wrote the change. No AI attribution of any kind — no bot
  trailers, no "Generated with" footers, no session links.
- `main` is protected: no direct pushes, admins included. All changes land
  via pull request.
- Merge method is squash-and-merge only; the head branch is deleted on merge
  (automatic). Keep PR titles in conventional-commit form — the squash commit
  inherits them.
- Linear history is enforced; no merge commits, no force pushes to main.
- A commit compiles and its tests pass. No WIP commits on main.
- Commit and push only what the task requires; design artifacts stay local
  (see Documentation policy).
