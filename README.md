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

`codingtools` defines the provider-neutral wire arguments and results for
`read`, `write`, `edit`, `bash`, `grep`, `find`, and `ls`. Each tool exposes a
hand-authored JSON input schema through a defensive-copy accessor and has a
bounded model-facing description. File paths are workspace-relative and file
tools reject escapes. Bash accepts an optional workspace-relative initial
directory, but commands retain host-process authority. Read results report the
actual returned line window and total file line count, including when the
requested window reaches EOF. The `read` tool accepts regular UTF-8 text files
up to 5 MiB by default, records a full-file content hash, and returns numbered
lines through the shared output policy and per-session budget. Existing files
must be read before write or edit, and stale reads refuse mutation. Writes and
edits are globally serialized, replace files through a synced same-directory
temporary file and atomic rename, and preserve existing permissions. Writes
create new files with mode `0644`; edit accepts only existing regular files.
Result shapes are JSON objects and reserve `diagnostics` and `diagnostics_text`
for later LSP decoration.

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

Engineering standards: AGENTS.md.
