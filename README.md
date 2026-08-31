<p align="center">
  <img src="./assets/plasmid-hero.png" alt="An abstract circular plasmid assembled from glowing modular segments" width="100%">
</p>

<h1 align="center">Plasmid</h1>

<p align="center">
  <strong>Put a coding agent inside your Go app.</strong><br>
  Native Google ADK, durable sessions, coding tools, and no daemon to babysit.
</p>

<p align="center">
  <a href="https://github.com/RandomCodeSpace/plasmid/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/RandomCodeSpace/plasmid/ci.yml?branch=main&style=for-the-badge&label=CI"></a>
  <a href="https://pkg.go.dev/github.com/RandomCodeSpace/plasmid"><img alt="Go reference" src="https://img.shields.io/badge/Go-reference-00ADD8?style=for-the-badge&logo=go&logoColor=white"></a>
  <a href="https://github.com/RandomCodeSpace/plasmid/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/RandomCodeSpace/plasmid?style=for-the-badge"></a>
  <a href="./LICENSE"><img alt="MIT license" src="https://img.shields.io/github/license/RandomCodeSpace/plasmid?style=for-the-badge"></a>
</p>

Plasmid is for Go applications that need an agent in the same process, using
the same types and lifecycle as the rest of the program. Bring a native Google
ADK model, point Plasmid at a workspace, and run a turn.

It is a library, not a CLI wrapper. There is no subprocess protocol, background
daemon, RPC server, or second agent framework hiding underneath it.

## What you get

- Native Google ADK models, tools, events, callbacks, and sessions.
- A durable coding Harness with `read`, `write`, `edit`, `grep`, `find`, `ls`,
  and optional `bash`.
- A bounded one-shot runner when the model must see exactly the tools you pass.
- Compatible instructions and skills already installed for Claude Code, Codex,
  and GitHub Copilot.
- Lazy LSP diagnostics after successful edits. Plasmid detects language servers
  but never installs them.
- Explicit OpenAI Responses and Chat Completions adapters with caller-owned HTTP
  policy.

## Pick the right mode

| If you need...                                | Use                                                                                |
| --------------------------------------------- | ---------------------------------------------------------------------------------- |
| One bounded call with an exact tool list      | [`oneshot.Run`](https://pkg.go.dev/github.com/RandomCodeSpace/plasmid/oneshot#Run) |
| A durable agent that can work in a repository | [`plasmid.New`](https://pkg.go.dev/github.com/RandomCodeSpace/plasmid#New)         |

One-shot performs no discovery or persistence. The Harness adds coding tools,
durable sessions, compatible agent context, optional MCP tools, LSP feedback,
and compaction.

## Install

Plasmid requires Go 1.26.6 or newer.

```sh
go get github.com/RandomCodeSpace/plasmid@latest
```

Any native `google.golang.org/adk/v2/model.LLM` works. The example below uses
Plasmid's OpenAI Responses adapter.

## Run your first durable agent

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "os"
    "time"

    "github.com/RandomCodeSpace/plasmid"
    "github.com/RandomCodeSpace/plasmid/openai"
)

func main() {
    ctx := context.Background()

    llm, err := openai.New(ctx, openai.Config{
        Protocol:         openai.ProtocolResponses,
        Model:            os.Getenv("OPENAI_MODEL"),
        BaseURL:          "https://api.openai.com/v1/",
        APIKey:           os.Getenv("OPENAI_API_KEY"),
        HTTPClient:       &http.Client{Timeout: 60 * time.Second},
        MaxResponseBytes: 8 << 20,
        MaxRetries:       0,
    })
    if err != nil {
        log.Fatal(err)
    }

    agent, err := plasmid.New(ctx,
        plasmid.WithModel(llm),
        plasmid.WithWorkingDir("."),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer func() {
        if err := agent.Close(); err != nil {
            log.Printf("close Plasmid: %v", err)
        }
    }()

    sessionID, err := agent.NewSession(ctx)
    if err != nil {
        log.Fatal(err)
    }

    answer, err := agent.Ask(ctx, sessionID, "Explain this repository to a new contributor.")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(answer)
}
```

Sessions default to `.plasmid/sessions` inside the working directory. Keep the
returned ID and call `ResumeSession` when the host starts again. Use `Run`
instead of `Ask` when the host needs the native ADK event stream.

Plasmid's OpenAI adapter ignores ambient `OPENAI_*` values. The example reads
the two values deliberately and passes them in. Endpoint, credentials, retry
count, HTTP client, and response limit stay under host control.

## Run one bounded call

Use one-shot when a request should have no workspace discovery, durable state,
or surprise tools.

```go
func runOnce(
    ctx context.Context,
    llm model.LLM,
    tools []tool.Tool,
) (oneshot.Result, error) {
    return oneshot.Run(ctx, oneshot.Request{
        Model:                   llm,
        Instruction:             "Use only the supplied tools.",
        Prompt:                  "Look up the current value and explain it.",
        Tools:                   tools,
        MaxOutputTokens:         1024,
        MaxReturnedTextBytes:    64 << 10,
        MaxModelCalls:           4,
        MaxToolCallsPerResponse: 8,
        ToolExecution:           oneshot.ToolExecutionSequential,
    })
}
```

All four limits are required. Tool calls run sequentially unless the host opts
into `ToolExecutionParallel`. A non-nil error can still include bounded partial
text, completed tool results, and usage in the returned `Result`.

Need to check a provider before a real run? `oneshot.Probe` makes one inert
tool-calling request and executes nothing.

## Reuse the agent setup you already have

The durable Harness can read compatible, already-installed inputs from:

- Claude Code
- Codex
- GitHub Copilot

That includes supported instruction files, Agent Skills, legacy templates, and
MCP declarations. Discovery is inert. It reads bounded metadata and never runs
hooks, commands, plugins, MCP servers, or language servers.

Repository content starts untrusted. Add a concrete trusted directory only
after the host has approved it. Foreign MCP declarations also need explicit
consent before Plasmid connects.

## Configuration is optional

Functional options are enough for a small host. Add a workspace
`.plasmid.json` when several settings should travel together:

```json
{
  "version": 1,
  "lsp": {
    "mode": "auto"
  },
  "foreign": {
    "enabled": true,
    "trustedRoots": []
  },
  "mcp": {
    "inheritForeign": false,
    "allowForeign": []
  },
  "compaction": {
    "contextTokens": 0
  }
}
```

Plasmid checks the workspace file first, then XDG and user config locations.
Files do not merge. Functional options win over the selected file. See the
[`config` package](https://pkg.go.dev/github.com/RandomCodeSpace/plasmid/config)
for every setting and default.

## A blunt security note

Plasmid is in-process, so it has the authority of its host.

- File tools stay inside the configured workspace root.
- `bash` and prompt commands are bounded, but they are not sandboxed. They keep
  the host process's filesystem, environment, credentials, and network access.
- Plasmid never installs skills, plugins, MCP servers, or language servers.
- Durable transcripts may contain prompts and tool results. Protect the session
  directory as application data.

If the agent needs isolation, isolate the host process. There is no clever
library trick that substitutes for an operating-system boundary.

## Project status

Plasmid is pre-v1. The public Go interface and durable session format may still
change between minor releases.

V1 is Go-only and single-agent. There is no Plasmid CLI, subagent runtime, plan
mode, permission popup, background shell, marketplace, or summarizing
compactor. Those are product boundaries, not hidden configuration switches.

## Reference

- [Go package reference](https://pkg.go.dev/github.com/RandomCodeSpace/plasmid)
- [One-shot package](https://pkg.go.dev/github.com/RandomCodeSpace/plasmid/oneshot)
- [OpenAI model adapters](https://pkg.go.dev/github.com/RandomCodeSpace/plasmid/openai)
- [Releases](https://github.com/RandomCodeSpace/plasmid/releases)
- [Engineering and contribution rules](AGENTS.md)

## Development

```sh
go test ./...
go build ./...
go vet ./...
go run ./internal/fixture/cmd/fixturepack
```

CI also runs the race suite, module verification, Windows compilation, fixture
regeneration, vulnerability scanning, and secret scanning.

## License

[MIT](LICENSE)
