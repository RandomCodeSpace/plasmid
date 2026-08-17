# plasmid

A CLI-free, in-process coding-agent harness for Go, built on Google ADK.
Reads the skills and plugins your other agent tools already installed.

Plasmid requires Go 1.26.6 or newer and directly pins Google ADK v2.2.0.
The direct Google ADK integration is the v1 runtime contract. There is no
provider-neutral loop or adapter package.

## Native Harness

`plasmid.New` constructs a native ADK `llmagent` and `runner`, six filesystem
coding tools, optional `bash` when a shell is available, and a durable session
service in process. A model is required. The
working directory defaults to the resolved current directory, and sessions
default to `<workingDir>/.plasmid/sessions`.

```go
p, err := plasmid.New(ctx,
    plasmid.WithModel(model),
    plasmid.WithWorkingDir(workdir),
    plasmid.WithSessionDir(sessiondir),
)
if err != nil {
    return err
}
defer p.Close()

sessionID, err := p.NewSession(ctx)
if err != nil {
    return err
}
answer, err := p.Ask(ctx, sessionID, "Inspect the repository")
```

`Run` exposes native `iter.Seq2[*session.Event, error]` events. It permits one
active run per session while allowing distinct sessions to run concurrently.
Stopping iteration early cancels the run and releases its session lock.
`ResumeSession` verifies an existing durable session and never creates a
missing one. `Ask` returns text from the last final root-agent event.

Construction is transactional. `Close` is concurrent-safe and idempotent: it
cancels active runs and waits up to ten seconds for reverse-order teardown of
compiled plugins, native ADK plugins, and the session store. If a run resists
cancellation, teardown proceeds after the wait and `Close` returns a coded
error matching `ErrCloseTimeout`; concurrent and repeated calls observe the
same completed teardown. Runtime failures use `plasmid.Error` with stable
`ErrorCode` values; `CodeOf` extracts the code while `errors.Is` continues to
match the exported sentinel cause.

Host tools use `WithTools`. Native ADK plugins use `WithADKPlugins`; their
callback mutation and short-circuit semantics remain authoritative. A compiled
`Plugin` may register tools, toolsets, and ADK callback bundles during `Init`;
registration seals before `New` returns, and callbacks are installed exactly
once in deterministic registration order.

## Context and syntax runtime

Each session snapshots supported Codex, Claude Code, and GitHub Copilot
instruction files. User and ancestor files assemble before repository-root
files; nested `AGENT.md`, `AGENTS.md`, and `CLAUDE.md` files and path-scoped
Claude and Copilot rules activate only after a native coding tool touches a
matching path. Activation is session-local, and later model steps receive the
updated least-specific-to-most-specific prompt.

Instruction discovery is cancellation-aware and bounded. Real-path and
content-hash deduplication remove duplicate sources, Claude `@path` imports are
confined to the workspace, the user home for user instructions, and configured
import roots. The assembled byte budget evicts least-specific content first
with a structured warning. Session, project-directory, and static effort
variables come from Harness state; the process environment is not an implicit
substitution source.

Instruction frontmatter can restrict native tool names and arguments with
deny-wins nested policy intersection. Tool names exposed to the model are
filtered through the active policy, and a native before-tool callback rechecks
the complete argument before execution. Host names such as `Read`, `Glob`, and
`LS` map deterministically to `read`, `find`, and `ls`. Turn scopes are released
after normal completion, model or tool errors, cancellation, and early stream
termination.

Inline `!` commands and fenced command directives expand only through the same
bounded `shellexec` executor used by `bash`. `syntax.promptCommands` is `off`,
`trusted`, or `on`; the default `trusted` mode runs user instructions and
repository instructions beneath an exact `foreign.trustedRoots` entry. Each
command and document has independent time and output limits. Instruction
discovery itself never runs commands.

`sessionstore` is the native durable Google ADK `session.Service`. Its
per-session JSONL transcript is the commit record for complete non-partial ADK
events and session-local state. App and user state use independent append-only
journals with repairable derived snapshots. Temporary state is never persisted.
The store serializes operations within one session while allowing different
session transcripts to progress concurrently, and enables file and directory
durability barriers by default. Its on-disk format is pre-v1 and carries no
compatibility guarantee.

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

## Deterministic compaction

Setting `compaction.contextTokens` above zero installs one native ADK
before-model callback and one after-model callback. The before-model callback
estimates the assembled native request, reapplies durable sticky decisions,
and compacts only when the calibrated estimate reaches the configured trigger.
The after-model callback calibrates future estimates from reported prompt usage
with EWMA alpha `0.3`, clamped to `0.5` through `2.0`. No provider-neutral
request facade or summarizer is involved.

The raw estimator is fixture-pinned: canonical JSON uses sorted object keys,
does not HTML-escape text, and charges one token per four UTF-8 bytes rounded
up. It then adds 4 tokens per content, 1 per part, 8 per function call or
response, 16 per binary payload, and 12 per function declaration. These are
deterministic framing allowances, not claims about a provider tokenizer.

Compaction replaces the oldest eligible function or server-tool response body
with `[elided]` while retaining its ID, name or type, and pair. Configured tool
names are never body-elided, and turns containing them are never dropped. If
elision cannot reach the target, Plasmid drops
the oldest complete turn: a user prompt and its following model/tool traffic,
ending immediately before the next user prompt. Content index zero, the active
turn, the configured recent-content window, system instructions, and any turn
whose removal would split a call/response pair remain intact.

Elided response identities and dropped-turn fingerprints, including repeated
identical response and turn decisions, persist in the session's versioned
`compaction.v1` sidecar and reapply after restart. Sidecar
load or save failure warns once and continues with in-memory state. A triggered
compaction resets that session's cumulative tool-output budget. If protected
content still exceeds the target, one exhaustion warning is recorded and the
model call proceeds.

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
are JSON objects and reserve `diagnostics` and `diagnostics_text` for LSP
decoration.

## LSP enforcement

Automatic LSP mode subscribes to the shared workspace touch stream. The first
successful `write` or `edit` of a matching file lazily detects and starts one
server per resolved workspace root and server ID, sends full-text `didOpen` or
monotonically versioned `didChange`, and waits within `settleTimeoutMs` for a
diagnostic publication for that exact document generation. The native ADK
after-tool callback then adds only `diagnostics` and `diagnostics_text` to that
invocation's successful result. Stale publications are ignored; an explicit
current empty publication clears diagnostics. Read tools, failed mutations,
and unrelated invocations are never decorated.

The prompt reports `LSP: none detected` before a matching server starts and a
sorted list of active server IDs afterward. `mode: "off"` omits the status,
manager, subscription, and callback entirely. The optional model-facing
diagnostics, symbols, and references query tools are not registered by this
release.

The immutable registry includes `gopls` and accepts validated per-ID overrides.
Executable detection is lazy through `exec.LookPath`; Plasmid never downloads
or installs a language server. Servers use bounded Content-Length JSON-RPC over
stdio and degrade unavailable, failed, timed-out, or exited processes to a
structured warning and no-op. Request deadlines interrupt blocked transport
I/O, and cancellation or Harness close terminates the owned server process tree
on Unix and Windows. Other targets fail server startup rather than leave
descendants unmanaged.

The package also owns confined workspace-root selection, portable file URI
conversion, UTF-8 and UTF-16 position conversion, deterministic bounded
diagnostic normalization, full-text document versions, and fakeable transport
and process-start seams. The root Harness alone owns native ADK injection and
the LSP resource lifecycle.

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
