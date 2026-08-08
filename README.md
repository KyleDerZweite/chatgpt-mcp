# chatgpt-mcp

Experimental. A local MCP server that drives a Chrome session on your own
machine against a chatgpt.com page you are logged into. Written in Go, using
the MCP Go SDK and go-rod.

This is a personal project. It stops working whenever the chat page's DOM
changes, which happens without notice. It is not a product, not supported,
and not affiliated with or endorsed by OpenAI.

## Status

Experimental. Expect breakage and frequent selector fixes. No releases, no
tags, no issues expected to be answered.

## What it does

The server launches Chrome with a dedicated, persistent profile (default
`~/.chatgpt-mcp/Profile`). You log into ChatGPT once, manually, in that
window. The server keeps the session and exposes four tools:

| Tool | Purpose |
|---|---|
| `chatgpt_ask` | Send a prompt, optionally selecting a model first; waits for the full response |
| `chatgpt_reply` | Follow-up prompt in the current conversation |
| `chatgpt_new_chat` | Start a fresh conversation |
| `chatgpt_upload` | Attach files, optionally with a prompt, and wait for the response |

All calls are sequential and human-paced. The server reads response text from
the page it drove; there is no API, no hosted component, and nothing runs on
any machine but yours.

## Terms and security notes

Read this before using.

* OpenAI's Terms of Use for ChatGPT prohibit automatic or programmatic
  extraction of data or Output from the service. This project automates the
  web interface rather than using the OpenAI API. Whether a given use is
  permitted depends on the terms that apply to your account. Verify before
  using it for anything.
* The server does not collect, extract, transmit, or store credentials or
  session tokens. Authentication is you, in your own browser, once.
* The project does not bypass CAPTCHAs, Cloudflare checks, authentication,
  rate limits, subscription gates, or any other technical access control.
  It opens a normal, sandboxed Chrome with default session behavior.
* Run it locally. Never expose the MCP endpoint to anything untrusted, and
  treat the browser profile like a password file — it contains your login
  session and it must not be shared, committed, or uploaded.

## Build

```sh
go build -o chatgpt-mcp .
```

## First run

Launch the server from your MCP client. A Chrome window opens with the
dedicated profile; log into ChatGPT in it. The session stays in the profile
for later runs. If the page shows a login prompt, complete the login in the
window and the in-flight request continues.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `CHATGPT_MCP_DIR` | `~/.chatgpt-mcp/Profile` | Chrome profile directory (the login session lives here) |
| `CHATGPT_CDP_URL` | empty | When set, attach to a Chrome already running at that CDP address instead of launching one |
| `CHATGPT_HEADLESS` | `false` | Run Chrome headless |
| `CHATGPT_CHROME_BIN` | auto-detect | Path to a Chrome/Chromium binary |
| `CHATGPT_TIMEOUT_MINUTES` | `30` | Default max wait for a single ask |
| `CHATGPT_MAX_TIMEOUT_MINUTES` | `120` | Upper bound for a single ask |
| `CHATGPT_SCREENSHOTS` | `true` | Save a screenshot to `CHATGPT_DEBUG_DIR` when something fails |
| `CHATGPT_DEBUG_DIR` | `~/.chatgpt-mcp/debug` | Where failure screenshots go |

## Client configuration

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

Other MCP clients use their equivalent of:

```json
{
  "mcpServers": {
    "chatgpt": { "command": "/path/to/chatgpt-mcp" }
  }
}
```

## Models

`chatgpt_ask` has an optional `model` argument supplied to the chat page's
model parameter when given. Which values work depends on the page and on
your account; recent slugs included `gpt-5`, `gpt-5-thinking`, `gpt-5-pro`,
`gpt-4-1`, `o3`, but treat any of them as temporary.

## Known limits

- The DOM changes constantly. Completion is guessed from a copy-action
  button appearing in the newest turn, with a content-stability fallback and
  a thinking-phase guard. Expect to patch selectors regularly. Failure
  screenshots in `~/.chatgpt-mcp/debug/` show what the page actually looked
  like.
- Only plain response text; no images, code artifacts, or tool outputs.
- One Chrome instance and one account; concurrent client requests are
  serialized.
- The consumer web product may add checks (such as a "verify you are
  human" step) at any time. Sometimes there is nothing to do but wait, or
  clear the profile and log in again.