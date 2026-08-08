# chatgpt-mcp

MCP server that exposes the ChatGPT web interface to MCP clients (Claude, opencode, etc.) as a blocking `ask → response` tool. Written in Go with the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) and [go-rod](https://github.com/go-rod/rod) (CDP).

Built for the case where the subscription/Pro allowance you want (e.g. GPT-5.x Pro-tier models) is only reachable through chatgpt.com, not the API.

## Tools

| Tool | Purpose |
|---|---|
| `chatgpt_ask` | Send a prompt (optionally switch model first) and wait for the full response. Blocking. |
| `chatgpt_reply` | Follow-up in the current conversation. Blocking. |
| `chatgpt_new_chat` | Start a fresh conversation. |
| `chatgpt_upload` | Upload files (+ optional prompt) and wait for the response. |

## Build

```sh
go build -o chatgpt-mcp .
```

## First run (one-time login)

The server launches its own Chrome with a **dedicated profile** (default `~/.chatgpt-mcp/Profile`). Log into ChatGPT in that window once; the session persists across runs.

Optional: attach to an already-running Chrome instead (e.g. launched with
`--remote-debugging-port=9222 --user-data-dir=~/.chatgpt-mcp/Profile`) by setting `CHATGPT_CDP_URL`.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `CHATGPT_MCP_DIR` | `~/.chatgpt-mcp/Profile` | Chrome profile dir (login session lives here) |
| `CHATGPT_CDP_URL` | empty | If set, attach to a running Chrome at this CDP URL instead of launching one |
| `CHATGPT_HEADLESS` | `false` | Run Chrome headless |
| `CHATGPT_CHROME_BIN` | auto-detect | Path to the chrome binary |
| `CHATGPT_TIMEOUT_MINUTES` | `30` | Default max wait per ask |
| `CHATGPT_MAX_TIMEOUT_MINUTES` | `120` | Hard cap on per-ask wait |
| `CHATGPT_SCREENSHOTS` | `true` | Save a debug screenshot to `CHATGPT_DEBUG_DIR` on errors |
| `CHATGPT_DEBUG_DIR` | `~/.chatgpt-mcp/debug` | Debug screenshot location |

## Client config

opencode (`opencode.json`):

```json
{
  "mcp": {
    "chatgpt": {
      "type": "local",
      "command": ["/path/to/chatgpt-mcp"]
    }
  }
}
```

Claude Desktop / Claude Code:

```json
{
  "mcpServers": {
    "chatgpt": { "command": "/path/to/chatgpt-mcp" }
  }
}
```

## Model selection

`chatgpt_ask` accepts a `model` argument; model switching uses the URL parameter form (`/chatgpt?model=gpt-5`), which is far more stable than driving the dropdown. Example slugs observed with the subscription: `gpt-5`, `gpt-5-thinking`, `gpt-5-pro`, `gpt-4-1`, `o3`.

## Robustness / known fragility

- The ChatGPT DOM changes without notice. Completion is detected via the *copy action button appearing in the last conversation turn* (`[data-testid="copy-turn-action-button"]`), with a content-stability fallback and a thinking-phase guard — but selector fixes are to be expected over time. Screenshots on error (`~/.chatgpt-mcp/debug/`) show what the UI actually looked like.
- Responses are plain text extracted from the DOM; no images/artifacts.
- This drives the consumer web product. Keep usage sequential and human-speed, and prefer the API/Codex for large automated workloads.