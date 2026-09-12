# ADK v2.2.0 callbacks on model error and cancellation

Resolves RandomCodeSpace/plasmid#86. Every claim below is traced to the pinned
module source at `$GOMODCACHE/google.golang.org/adk/v2@v2.2.0` (`go.mod:12`)
or to the Plasmid working tree at `main` (`b66c918`). Paths without a prefix
are Plasmid files; `adk/` prefixes the module root.

## Execution path Plasmid actually takes

Plasmid's root agent is a chat-mode `llmagent` driven by `runner.Run`
(`harness.go:194`). For an LlmAgent root the runner does **not** call
`rootAgent.Run` directly; it takes the node path:

| Step | Source |
|---|---|
| `runner.Run` detects an LlmAgent root and dispatches to `runNode` | `adk/runner/runner.go:199-248` |
| `runNode` wraps the agent in a single-node workflow, runs `on_user_message`, defers `after_run`, runs `before_run`, then consumes `wf.Run` | `adk/runner/run_node.go:96-200` |
| The workflow scheduler runs the node body on its own goroutine (`go runNode(...)`) with a `recover()` that converts a panic into a completion error | `adk/workflow/scheduler.go:405`, `:434-500` (recover at `:452-454`) |
| Node body → `llmagent.RunLLMAgentAsNode` → `runChat` → `agent.Run` | `adk/runner/agent_node.go:88-130`, `adk/agent/llmagent/llm_agent_wrapper.go:65-102`, `:486-590` |
| `agent.Run` runs before-agent callbacks, the flow, then after-agent callbacks | `adk/agent/agent.go:162-210` |
| `llmagent.run` builds the `llminternal.Flow` with the agent-level callback slices | `adk/agent/llmagent/llmagent.go:440-465` |
| `Flow.Run` → `runOneStep` → `preprocess` (calls `Toolset.Tools`) → `callLLM` → `handleFunctionCalls` → `platform.RunTasks` → `callTool` | `adk/internal/llminternal/base_flow.go:103-131`, `:570-705`, `:707-731`, `:773-851`, `:1063-1238`, `:1251-1296`; `adk/platform/exec.go:68-95` |

Consequences that shape every row below:

- Tool calls (and the before/after/on-error tool callbacks) run inside
  `platform.RunTasks` goroutines, one per function call, and `RunTasks`
  blocks until all of them return (`adk/platform/exec.go:88-94`). The tool
  callbacks therefore always complete before the flow yields the function
  response event.
- Plasmid's `Harness.run` stops consuming on the first error
  (`harness.go:196-198`), and `runChat` returns on the first error before
  checking the yield result (`llm_agent_wrapper.go:519-522`). Either one
  makes `agent.Run`'s `yield` return `false` at `agent.go:197`, which
  returns before the after-agent callbacks at `agent.go:206`.
- The runner-level plugin manager Plasmid installs carries only the four
  run-level callbacks (`harness.go:687-696`, `harness_construction.go:440-456`).
  Plugin before/after model and tool callbacks are appended to the
  **agent-level** slices instead (`harness_construction.go:424-425`,
  `:499-523`), after Plasmid's own compaction and LSP callbacks
  (`harness_construction.go:462-484`). So every `pluginManager.Run*Model*`
  and `Run*Tool*` call in `base_flow.go` is a no-op for Plasmid, and Plasmid's
  own callbacks are always first in each agent-level chain.

## Callback kind x condition

Conditions: (a) model call returns an error, (b) `tool.Run` returns an
error, (c) run context cancelled while the model call is in flight, (d) run
context cancelled while a tool call is in flight. For (c) and (d) the
behaviour of Plasmid's model adapter matters: `openai.redactedModel`
yields `context.Canceled` as an error (`openai/openai.go:199-207`,
`:225-226`), so (c) is the same code path as (a). Plasmid's coding tools
observe `ctx.Err()` and return it (`codingtools/write.go:280`,
`edit.go:170`, `bash.go:190`, ...), so (d) is the same code path as (b)
inside the task goroutine.

"Fires" means the callback is invoked on that path in Plasmid's wiring.
"Before" callbacks fired before the failure are listed as fires.

| Callback | (a) model error | (b) tool error | (c) cancel mid-model | (d) cancel mid-tool | Source |
|---|---|---|---|---|---|
| OnUserMessage | fires (before the run) | fires | fires | fires | `adk/runner/run_node.go:97` → `runner.go:613-621` |
| BeforeRun | fires (before the run) | fires | fires | fires | `adk/runner/run_node.go:113` |
| AfterRun | **fires** (deferred) | fires | **fires** (deferred) | **fires** (deferred) | `adk/runner/run_node.go:111` (`defer`), `plugin_manager.go:110-117` |
| OnEvent | **does not fire** for the error; the error branch skips it | fires for the function-response event carrying `{"error": ...}` | **does not fire** | **may or may not fire**: the function-response event is sent with `select { out <- ev; <-ctx.Done() }` and the scheduler yields only while not draining | `adk/runner/run_node.go:154-158` (error branch), `:172` (event branch); `adk/workflow/scheduler.go:478-490`, `:572-590` |
| BeforeAgent | fires (before the flow) | fires | fires | fires | `adk/agent/agent.go:182` |
| AfterAgent | **does not fire**: error yield at `agent.go:197` returns `false` because `runChat` returns on error | fires (run completes normally) | **does not fire** (same path as (a)) | **does not fire**: the next step's model call returns `context.Canceled`, then (a) | `adk/agent/agent.go:196-208`; `adk/agent/llmagent/llm_agent_wrapper.go:519-522` |
| BeforeModel | fires (before the call) | fires (next step) | fires (before the call) | fires again for the next step if the FR event yield returned `true`; the model call then fails with `context.Canceled` | `adk/internal/llminternal/base_flow.go:785-793` |
| AfterModel | **does not fire** unless an OnModelError callback substitutes a response; then fires with `llmErr == nil` | fires (next step) | **does not fire** (same as (a)) | fires only for the completed model steps before the cancel | `base_flow.go:801-822`: error → `runOnModelErrorCallbacks` `:803`; `cbErr` → `yield(nil, cbErr)` `:804-806`; `cbResp == nil` → `yield(nil, err)` `:808-810`; substitute → `err = cbErr` (`nil`) `:816` then `runAfterModelCallbacks` `:822` |
| OnModelError | **fires** with the model error | does not fire | **fires** with `context.Canceled` | fires for the next step's model call if reached | `base_flow.go:803`, `:930-950` |
| BeforeTool | does not fire (no tool step) | fires | does not fire | fires; Plasmid's policy callback returns `ctx.Err()` if already cancelled, which skips `tool.Run` and takes the error path | `base_flow.go:1254-1260`; `harness_construction.go:481-483`; `tool_guard.go:214-217` |
| AfterTool | does not fire | **fires**, unconditionally, same goroutine, with `result` = on-error substitute or `nil` and `err` set | does not fire | **fires** (same as (b)) | `base_flow.go:1279-1290`; agent chain `:1313-1327` |
| OnToolError | does not fire | **fires** before AfterTool | does not fire | **fires** | `base_flow.go:1266-1277`; agent chain `:1329-1343` |

Additional tool-path facts:

- A before-tool error skips `tool.Run` but still runs OnToolError and
  AfterTool with that error (`base_flow.go:1258-1286`).
- Unknown tool name: only OnToolError fires, with a `fakeTool`
  (`base_flow.go:1110-1115`, `:1171-1176`). No BeforeTool or AfterTool.
- Streaming tools (`RunStream`) run with **no** tool callbacks at all
  (`base_flow.go:1158-1170`). Plasmid's write/edit tools are function tools,
  so this does not affect the LSP receipt path.
- Agent-level chains stop at the first callback returning a non-nil result
  or error (`base_flow.go:1298-1343`, `:908-950`). Plasmid's compaction and
  LSP callbacks always return `nil, nil` and are first in their chains, so a
  plugin callback cannot pre-empt them.
- In v2.2.0 AfterModel never receives a non-nil `llmResponseError`: the
  only assignment on the error path is `err = cbErr` at `base_flow.go:816`,
  and `cbErr` is known nil there (`:804`). The `responseError != nil` guard
  at `compaction/manager.go:125` is therefore dead code (harmless).

Error surfacing: a flow error becomes the node's completion error
(`scheduler.go:465-468`), is yielded once by the scheduler after all node
goroutines return (`scheduler.go:632-638`), reaches `run_node.go:154-158`,
and is wrapped by Plasmid as `CodeRuntimeFailed` (`harness.go:196`,
`:510-520`). On cancellation the scheduler reports `context.Cause(parentCtx)`
instead of the node's echo of it (`scheduler.go:576-580`, `:836-844`).

## Is `Toolset.Tools` ever invoked on a goroutine outside the harness's recover seams?

Call sites of `Toolset.Tools` in the module (non-test):

| Site | Goroutine |
|---|---|
| `adk/internal/llminternal/tools_processor.go:41` (`toolProcessor`, a request processor run by `preprocess`) | the workflow node goroutine started at `adk/workflow/scheduler.go:405` |
| `adk/tool/tool.go:106`, `:153` (confirmation / filtered toolset wrappers delegating to the inner toolset) | same goroutine as their caller (above) |
| `adk/internal/llminternal/base_flow.go:299` (`RunLive` preprocess goroutine) | separate goroutine, **no recover**; Plasmid never calls `RunLive` (`harness.go:194` uses `Run`) |

Plasmid's own call sites: `harness.go:824` (`scopedToolset.Tools` →
`source.Tools`), `skills/toolset.go:115` (from `ProcessRequest`, run by
`toolsetPreprocess` at `base_flow.go:762`, same goroutine), and
`tool_guard.go:188` (`WithConfirmation(...).Tools(nil)`, reached from
`guardToolExecution` inside `scopedToolset.Tools`/`ProcessRequest`, same
goroutine). `safeCanonicalToolsDict` (`llm_agent_wrapper.go:368-384`) reads
only the static `Tools` slice and never calls a toolset.

Answer: **no.** Every `Toolset.Tools` invocation on Plasmid's path runs on
the workflow node goroutine, whose body is wrapped by the scheduler's
`recover()` (`scheduler.go:452-454`). A panic there is converted to the
completion error `node %q panicked: %v`, surfaced as a normal run error, and
wrapped by Plasmid as `CodeRuntimeFailed`. It never escapes the process and
never escapes `Harness.Run`. Note the recovery is ADK's, not Plasmid's:
Plasmid's own seams cover plugin callbacks (`plugin_callbacks.go:94-107`),
the LSP after-tool callback (`lsp_callback.go:29-43`), plugin
`Init`/`Close`/`Name` (`harness.go:628-682`), and static names
(`registry.go:278-285`); none wrap `Tools`. It is not called from a
`RunTasks` goroutine.

Related report-only finding (outside the ticket's question): `tool.Run`
executes on `platform.RunTasks` goroutines (`adk/platform/exec.go:88-92`)
that have **no** recover in ADK (`base_flow.go:1083-1230`) and none in
Plasmid (`tool_guard.go:52-65`, `codingtools/`). A panicking tool crashes
the host process. The LSP after-tool callback's own recover
(`lsp_callback.go:24`) protects only itself.

## Plasmid assumptions checked

### compaction/ — pending-estimate bookkeeping — VIOLATED

- Entry: `compaction/manager.go:112-116` (`before`, reached from
  `BeforeModel` at `:70-77`) stores `m.pending[app\0user\0session\0invocation]`.
- Release: only `compaction/manager.go:121-124` (`after`, reached from
  `AfterModel` at `:80-87`). `Manager` has no `Close`, eviction, or
  `OnModelError` hook; `grep pending compaction/` finds nothing else.
- Wiring: `harness_construction.go:479-480` registers `BeforeModel` and
  `AfterModel` only.

ADK does not run AfterModel on (a) model error or (c) cancellation
mid-model-call (`base_flow.go:803-810`), nor when a later agent-level
BeforeModel callback (a plugin's, appended after the compactor's at
`harness_construction.go:424-425`) short-circuits with a response or error
(`base_flow.go:785-793`, the loop returns before `:822`). Each such
invocation leaves one orphaned `int` in `m.pending` for the harness's
lifetime. Correctness is unaffected (same-invocation retries overwrite the
same key; other invocations use different keys), but the map grows without
bound under repeated model failures or cancellations. Graduated to a fix
ticket on the map.

Smallest fix: key `pending` by session (`identity.sessionKey()`) instead of
session+invocation. `before`/`after` for one session are strictly sequential
(`Harness` admits one active operation per session, `harness.go:446-454`),
so a session-keyed entry is always rewritten by the next `before` before any
`after` can read it, and the map is bounded by the session count exactly like
`m.sessions`. Optionally also register a `Manager.OnModelError` that deletes
the entry. Existing test to extend:
`compaction/manager_test.go:124-134`
(`TestManagerDoesNotTrackPendingUsageWhenCalibrationIsDisabled`).

### lsp/ — diagnostic receipt bookkeeping — NOT violated

- Entry: `lsp/enforcer.go:120-145` (`ObserveTouch`) stores a receipt keyed by
  `{touch.SessionID, touch.InvocationID}`. Touches are published
  synchronously from inside `tool.Run` (`workspace/touch.go:83-84`;
  `codingtools/write.go:100`, `codingtools/edit.go:93`) with
  `InvocationID = ctx.FunctionCallID()` (`codingtools/native.go:32`), the
  same key `Await` uses (`lsp_callback.go:53`).
- Release: `Await` (`lsp/enforcer.go:178-185`) or `Drop`
  (`lsp/enforcer.go:227-233`), both called from `lspAfterToolCallback`
  (`lsp_callback.go:21-65`); `Close` clears the map (`lsp/enforcer.go:259`).
- Wiring: `harness_construction.go:472-474`, first in the agent-level
  AfterTool chain.

AfterTool fires unconditionally after `tool.Run` returns, on the same
goroutine, for success, tool error, and cancellation
(`base_flow.go:1263-1290`), and the agent-level chain cannot be pre-empted
before Plasmid's callback because it is first (`base_flow.go:1313-1327`).
`ObserveTouch` also refuses to create a receipt when `ctx.Err() != nil`
(`lsp/enforcer.go:123`). The only path that skips AfterTool after a touch is
a panic inside `tool.Run`, which kills the process (see above), so the
receipt cannot outlive the harness. Assumption holds.
