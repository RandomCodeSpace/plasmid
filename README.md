# plasmid

A CLI-free, in-process coding-agent harness for Go, built on Google ADK.
Reads the skills and plugins your other agent tools already installed.

Plasmid requires Go 1.26.6 or newer and directly pins Google ADK v2.2.0.
The direct Google ADK integration is the v1 runtime contract. The current
`loop` and `adkloop` packages are temporary pre-v1 compatibility scaffolding
for the migration and are not normative; they will be removed before v1.

## Pre-v1 compatibility scaffolding

The legacy `loop` contract and its `adkloop` adapter exist only while the
implementation migrates to native ADK tools, sessions, events, and lifecycle
ownership. Hosts must not build new integrations against these packages.

All loader degradation uses the framework-free `warning.Warning` shape and
namespaced warning codes. Warnings are observable through a `warning.Sink`:
`warning.SlogSink` emits structured records and is the production fallback when
a sink is nil, `warning.DiscardSink` ignores warnings only when explicitly
selected, and `warning.SliceSink` collects defensive copies for callers and
tests. `Warning.String` renders as `<path>:<line>: <code>: <message>`; the
structured fields, not the rendered line, are the machine-readable contract.

## Versioned configuration

`config.Load` is the sole owner of Plasmid configuration. It accepts a context
and honors cancellation during discovery, bounded reads, decoding, and path
repair. It loads JSON version 1 from an explicit path when supplied; otherwise
it checks
`<workingDir>/.plasmid.json`, `$XDG_CONFIG_HOME/plasmid/config.json`, and
`~/.config/plasmid/config.json` in that order. The first existing file wins and
files never merge. Built-in defaults are applied first, the file overlays them,
and embedding overrides in `config.Options` apply last.

The file accepts these top-level keys:

```json
{
  "version": 1,
  "appName": "plasmid",
  "lsp": {},
  "mcp": {},
  "skills": {},
  "foreign": {},
  "syntax": {},
  "context": {},
  "tools": {},
  "compaction": {}
}
```

Block keys are:

- `lsp`: `mode`, `settleTimeoutMs`, `initializeTimeoutMs`,
  `requestTimeoutMs`, `failureThreshold`, `maxDiagnosticsPerFile`,
  `diagnosticsTool`, `symbolsTool`, `referencesTool`, and `servers`. Server
  entries use `id`, `command`, `args`, `extensions`, `rootMarkers`, and
  `disabled`; entries merge with the built-in gopls server by `id`.
- `mcp`: `inheritForeign`, exact `allowForeign` names, and `servers`. A server
  is either `stdio` with `id`, `command`, optional `args` and `env`, or `http`
  with `id`, `url`, and optional `headers`.
- `skills`: `roots`. `foreign`: `enabled`, `claude`, `codex`, `copilot`, and
  `trustedRoots`.
- `syntax`: `promptCommands`, `commandTimeoutMs`, `documentTimeoutMs`,
  `commandOutputBytes`, and `documentOutputBytes`.
- `context`: `maxFileBytes`, `maxBytes`, `maxImportDepth`, `importRoots`, and
  `touchesPerToolCall`.
- `tools`: `callOutputBytes`, `sessionOutputBytes`, `bashTimeoutMs`,
  `bashMaxTimeoutMs`, and `confirmation`.
- `compaction`: `contextTokens`, `triggerFraction`, `targetFraction`,
  `keepRecentContents`, `minimumElisionTokens`, `preserveToolNames`, and
  `calibration`. A zero context budget disables compaction.

Relative file paths are anchored to the selected config file and `~/` uses the
resolved home directory. Unknown keys and invalid optional values produce
stable structured warnings; invalid entries are repaired or dropped locally.
Malformed JSON and versions newer than version 1 fail loading. Version zero is
upgraded with a warning. Configuration loading performs bounded file I/O only:
it does not start configured LSP or MCP processes.

## Foreign extension discovery

The `foreign` package discovers metadata already present for Claude Code,
Codex, and GitHub Copilot. `foreign.Scan` returns a normalized skill view plus
three independently ordered host source views; `ScanClaude`, `ScanCodex`, and
`ScanCopilot` expose the host adapters separately. Shared portable skills merge
their host provenance when their logical identity and real path or bytes match.
Unrelated plugin-qualified records remain distinct. Records include source
scope, plugin identity and version, enabled state, repository trust, and
documented or compatibility classification. An unqualified skill name present
in distinct records across hosts is reported as ambiguous rather than resolved
through an invented host priority.

Only the Agent Skills `SKILL.md` core is shared. Each adapter owns its host's
skill roots, legacy templates, plugin manifests, configuration, and precedence.
Compatibility inputs are labeled, including Claude's version 2 installed-plugin
index, `$CODEX_HOME/skills`, and legacy Codex prompts. Copilot IDE prompt files
are preview input and remain disabled unless `Options.EnableCopilotPreview` is
set. Repository records are listed with their trust state; trusted-project MCP
configuration is read only when `Options.ProjectTrusted` is true.

Discovery is cancellation-aware, bounded file I/O. It never installs or runs
skills, plugins, hooks, commands, MCP servers, or network clients. Foreign MCP
records expose only inert identity and transport metadata; credentials,
headers, environment values, and arguments are deliberately absent. Activation
and MCP authorization are separate harness responsibilities.

## Output limiting

`outputlimit` provides deterministic UTF-8 and CRLF-safe output elision for
embedded hosts. `Policy.Apply` applies byte, line, and line-body limits and
uses the package's single `Marker` format. `ApplyLines` accepts logical lines
and applies the same rendering policy. `Writer` retains bounded state when its
policy has a positive byte cap; the zero-value policy is intentionally
unlimited. `Budget` coordinates per-session rendered-byte reservations without
exceeding its hard limit.

## Shell execution

`shellexec` runs a fresh non-interactive shell for each request. Each initial
working directory is resolved inside a `workspace.Root`, configured output
limits bound capture, and timeout or cancellation terminates the Unix process
group before escalating to a forced kill. A zero-value output policy is
unlimited. Stdout and stderr can be captured separately or connected to one
ordered stream with `RunMerged`. Shell execution is not an OS security boundary;
hosts requiring isolation must confine the host process.

## Coding tool contracts

`codingtools.New` returns the stable ordered set of native Google ADK function
tools: `read`, `write`, `edit`, optional `bash`, `grep`, `find`, and `ls`.
`Set.Tools` exposes native `tool.Tool` values directly. Each function tool has
an explicit hand-authored JSON input schema, an object response schema, and a
bounded model-facing description. Schema accessors return defensive copies.
The optional `bash` tool is omitted with a structured warning when no shared
shell executor is configured.

File paths are workspace-relative and file tools reject escapes. Bash accepts
an optional workspace-relative initial directory, but commands retain
host-process authority. Read results report the actual returned line window
and total file line count, including when the requested window reaches EOF.
The `read` tool accepts regular UTF-8 text files up to 5 MiB by default,
records a full-file content hash, and returns numbered lines through the shared
output policy and native ADK session identity. Existing files must be read
before write or edit, and stale reads refuse mutation. Writes and edits are
globally serialized, replace files through a synced same-directory temporary
file and atomic rename, and preserve existing permissions. Writes create new
files with mode `0644`; edit accepts only existing regular files. Result shapes
are JSON objects and reserve `diagnostics` and `diagnostics_text` for later LSP
decoration.

## LSP leaf foundations

`lsp` provides the framework-free lifecycle used by the later Harness
integration. Its immutable registry includes `gopls` and accepts validated
per-ID overrides. Executable detection is lazy through `exec.LookPath`; Plasmid
never downloads or installs a language server. Servers start once per resolved
workspace root and server ID, use bounded Content-Length JSON-RPC over stdio,
and degrade unavailable, failed, timed-out, or exited processes to a structured
warning and no-op. Request deadlines interrupt blocked transport I/O, and
cancellation or close terminates the owned server process tree on Unix and
Windows. Other targets fail server startup rather than leave descendants
unmanaged.

The package also owns confined workspace-root selection, portable file URI
conversion, UTF-8 and UTF-16 position conversion, deterministic bounded
diagnostic normalization, full-text document versions, and fakeable transport
and process-start seams. It does not yet decorate coding-tool results; that is
Harness integration rather than a leaf concern.

## Edit matching and diffs

`codingtools` applies edits deterministically: exact matches win first, then
matches that differ only in trailing spaces or tabs, then matches with one
uniform whitespace indentation delta. Ambiguous edits fail with every matching
line unless replacement of all matches is requested. Edits preserve a file's
UTF-8 BOM, dominant LF or CRLF line ending, and trailing-newline presence.
Matched ranges use half-open byte offsets in normalized, BOM-free source text.

Unified diffs use a deterministic line-level Myers edit script with three lines
of context by default. Path headers are emitted under `a/` and `b/`, missing
trailing newlines are marked in place, and excessive diff work falls back to a
single whole-file replacement hunk.

## Filtered directory walking

`codingtools` walks workspace descendants in deterministic lexical order and
returns slash-separated paths relative to the workspace root. Filters support
hidden and VCS directories, include and exclude globs, depth limits, bounded
visits and results, nested `.gitignore` files, and `.git/info/exclude`. The
supported ignore subset includes `*`, `?`, `**`, character classes, anchoring,
directory-only rules, comments, escapes, and last-match negation. Malformed
ignore rules are skipped with a warning. Symlinks are reported from link
metadata but are never descended, regardless of the compatibility
`FollowSymlinks` setting.

## Fixture conformance pack

`testdata/conformance/manifest-v1.json` and `fixtures-v1.tar` are the
language-neutral fixture contract. The manifest records the ordered fixture
areas and every archive entry's normalized slash-separated path, type, mode,
size, and SHA-256 hash. The USTAR archive fixes directory modes to `0755`,
regular file modes to `0644`, and timestamps and ownership to stable
values, so identical fixture inputs produce identical bytes on every platform.
Repository attributes preserve fixture sources byte-for-byte, including
intentional CRLF and binary inputs, and pin the JSON manifest to LF checkouts.

From the repository root, `go run ./internal/fixture/cmd/fixturepack` verifies
the committed artifacts without writing to them. Pass `-update` to regenerate
both files after an intentional fixture change. CI verifies the committed pack
and rejects regeneration that leaves a diff on Linux and Windows checkouts.
Static fixture ownership checks also require exactly one direct
`fixture.AssertCoverage` call from a runnable top-level test for every fixture
area.

Engineering standards: AGENTS.md.
