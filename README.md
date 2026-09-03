# chatgpt-mcp

> [!WARNING]
> This is experimental browser automation, not an official ChatGPT API client.
> ChatGPT page changes can break it without notice. Do not rely on it for
> unattended, high-stakes, or production work.

`chatgpt-mcp` is a local bridge that drives a Chrome/Chromium window on your
machine against an authenticated `https://chatgpt.com` session. It can expose a
stdio MCP server, an OpenAI Chat Completions-compatible HTTP provider for clients
such as OpenCode, or both transports from one process. It is a personal project
built with Go, the MCP Go SDK, and go-rod. It is not supported, affiliated with,
or endorsed by OpenAI.

## What it does

The bridge starts Chrome with a dedicated persistent profile (by default,
`~/.chatgpt-mcp/Profile`) or attaches to a Chrome DevTools Protocol endpoint.
Its MCP transport exposes four tools:

| Tool | Purpose |
| --- | --- |
| `chatgpt_ask` | Send a prompt, optionally requesting a model first, and wait for a new assistant response |
| `chatgpt_reply` | Send a follow-up prompt in the current conversation |
| `chatgpt_new_chat` | Start a fresh conversation and clear tracked conversation/model state |
| `chatgpt_upload` | Attach allowed local files and optionally send a prompt; disabled by default |

The HTTP provider implements a deliberately limited `/v1/chat/completions`
surface for local OpenAI-compatible clients. One browser and account are shared
by the process, and complete MCP and HTTP browser transactions are serialized. A
queued call honors cancellation, and MCP tool failures are returned as MCP errors
rather than successful results containing an error-shaped payload.

Parallel tab execution is intentionally not enabled yet. The safety invariants,
phased architecture, and test gates required before doing so are recorded in
[`docs/multi-tab-concurrency.md`](docs/multi-tab-concurrency.md).

## Security and terms

Read this before running the server:

- This project automates the consumer ChatGPT web interface. Review the terms
  and policies that apply to your account and intended use. The project does not
  bypass login, CAPTCHAs, rate limits, subscription gates, or other access
  controls.
- Treat every MCP or HTTP client connected to this process as fully trusted. It
  can type into your ChatGPT session, read responses, change conversations, and,
  through MCP if you explicitly enable uploads, send permitted local files to
  ChatGPT.
- Keep the bridge local. Its stdio transport has no authentication boundary of
  its own. The HTTP provider binds to `127.0.0.1` by default, rejects non-loopback
  `Host` values and all browser `Origin` requests, and can require a bearer key.
  Loopback is host-local, not user-local: on a shared machine, another local
  account or native process may be able to reach an unauthenticated listener.
  Set `CHATGPT_PROVIDER_API_KEY` even on loopback unless you control every local
  account and process that can connect to it.
  A non-loopback listener requires an explicit remote-access opt-in, a bearer
  key of at least 32 bytes, and a configured TLS certificate/private-key pair.
  The built-in HTTPS server requires TLS 1.2 or newer. Keep a reverse proxy and
  this bridge on the same host with the bridge bound to loopback if the proxy
  terminates TLS instead.
- Protect the Chrome profile as a secret: it contains your authenticated session.
  Failure screenshots can contain prompts, answers, filenames, and other account
  data. Neither should be committed or shared.
- Provider calls use ordinary ChatGPT Web conversations. Prompts and responses
  can appear in the account's ChatGPT history and follow its normal data controls
  and retention settings. The OpenAI API `store` option is not implemented and
  is rejected rather than treated as a privacy control.
- Sensitive actions validate the exact HTTPS `chatgpt.com` origin. Subdomains,
  lookalike hostnames, non-HTTPS pages, credentials in URLs, and explicit ports
  are rejected.
- Manual CDP endpoints are restricted to loopback by default. Remote CDP gives
  complete control of the authenticated browser: it requires an explicit opt-in
  and `https://` or `wss://`, and secure discovery may not downgrade to plain
  WebSocket. Prefer a loopback endpoint reached through a trusted secure tunnel.
- Do not manually type, send, or navigate in the automation tab while a tool is
  running, and do not share its CDP target with another controller. The server
  serializes MCP calls and detects target/conversation drift, but it cannot make
  unrelated browser input transactional.

## Requirements and build

- Go 1.25.14 or newer
- Chrome or Chromium
- A local MCP or OpenAI-compatible client
- A ChatGPT account that you can sign into interactively

From the repository root:

```sh
go build -o chatgpt-mcp .
```

On Windows, use `go build -o chatgpt-mcp.exe .`. Either put the resulting
executable on `PATH` or use its absolute path in your MCP client configuration.

## Run modes

```text
chatgpt-mcp                 MCP stdio server (backward-compatible default)
chatgpt-mcp mcp             MCP stdio server
chatgpt-mcp provider        OpenAI-compatible HTTP/HTTPS provider
chatgpt-mcp serve           Alias for provider mode
chatgpt-mcp both            MCP stdio and HTTP/HTTPS provider in one process
```

Provider modes listen at `http://127.0.0.1:8787` by default. Supplying both
provider TLS files changes the listener to HTTPS. `provider`, `serve`, and
`both` accept `--listen ADDRESS` to override `CHATGPT_PROVIDER_ADDR`:

```sh
./chatgpt-mcp provider
```

```powershell
.\chatgpt-mcp.exe provider
```

Chrome starts lazily on the first browser operation. Do not run separate MCP
and provider processes against the same profile or CDP target. Use `both` when
one local client needs both transports so they share the same serialized
browser worker.

## First run

1. Build the executable and configure your MCP or provider client.
2. Keep `CHATGPT_HEADLESS=false` for the initial login.
3. Call `chatgpt_ask` or send a provider completion. The first browser operation
   opens a Chrome window using the dedicated profile and navigates to
   `https://chatgpt.com`.
4. Complete the login in that window. If the tool reports that the interface was
   not ready or still requires login, finish signing in and retry the call.

When attaching over CDP, leave only the intended `chatgpt.com` tab open. Initial
selection fails when multiple matching tabs exist instead of choosing one
arbitrarily.

Set `CHATGPT_CDP_URL=auto` to attach to the exact Chromium instance recorded by
the dedicated profile's `DevToolsActivePort` file. Auto-discovery probes only
IPv4/IPv6 loopback, disables proxies and redirects, and verifies that the port
and browser-target path still match the profile file before connecting. Start
Chrome/Brave with `--remote-debugging-port=0` and the same
`--user-data-dir=CHATGPT_MCP_DIR`. An explicit CDP URL does not create or use the
profile directory.

The login session remains in `CHATGPT_MCP_DIR` for later runs. Do not use your
normal Chrome profile as the automation profile. If login or the page becomes
stuck, stop the bridge, inspect the dedicated browser window, and retry before
considering removal of the profile.

## Configuration

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `CHATGPT_MCP_DIR` | `~/.chatgpt-mcp/Profile` | Dedicated Chrome profile directory containing the login session |
| `CHATGPT_CDP_URL` | empty | Attach to an existing Chrome CDP endpoint instead of launching Chrome |
| `CHATGPT_CDP_ALLOW_REMOTE` | `false` | Permit an explicitly configured non-loopback `https://` or `wss://` CDP endpoint |
| `CHATGPT_HEADLESS` | `false` | Run Chrome headless; interactive login generally needs `false` |
| `CHATGPT_CHROME_BIN` | auto-detect | Chrome/Chromium executable path |
| `CHATGPT_DELAY_MS` | `1000` | Delay between composing and submitting a prompt; `0` disables it |
| `CHATGPT_TIMEOUT_MINUTES` | `30` | Default maximum wait for one browser completion |
| `CHATGPT_MAX_TIMEOUT_MINUTES` | `120` | Maximum accepted MCP operation timeout |
| `CHATGPT_SCREENSHOTS` | `false` | Opt in to failure screenshots, which can contain sensitive page content |
| `CHATGPT_DEBUG_DIR` | `~/.chatgpt-mcp/debug` | Private (`0700` where supported) failure screenshot directory; images are written `0600` |
| `CHATGPT_DEBUG_MAX_FILES` | `20` | Maximum retained bridge-owned failure screenshots |
| `CHATGPT_UPLOAD_ENABLED` | `false` | Enable the high-trust file-upload tool |
| `CHATGPT_UPLOAD_ALLOWED_ROOTS` | empty | Required upload roots, separated with the operating system's path-list separator |
| `CHATGPT_UPLOAD_MAX_FILES` | `5` | Maximum files in one upload |
| `CHATGPT_UPLOAD_MAX_FILE_BYTES` | `26214400` | Maximum size of one file (25 MiB) |
| `CHATGPT_UPLOAD_MAX_TOTAL_BYTES` | `52428800` | Maximum total size of one upload (50 MiB) |
| `CHATGPT_PROVIDER_ADDR` | `127.0.0.1:8787` | Provider listen address; plain HTTP by default on loopback |
| `CHATGPT_PROVIDER_API_KEY` | empty | Optional bearer key for local use; non-loopback keys must be at least 32 bytes |
| `CHATGPT_PROVIDER_MODELS` | `chatgpt-auto` | Comma-separated provider model registry |
| `CHATGPT_PROVIDER_DEFAULT_MODEL` | `chatgpt-auto` | Registered model used when a request omits `model` |
| `CHATGPT_PROVIDER_ALLOW_REMOTE` | `false` | Explicitly allow a non-loopback provider listen address |
| `CHATGPT_PROVIDER_TLS_CERT_FILE` | empty | PEM certificate chain for built-in HTTPS; must be set with the key file |
| `CHATGPT_PROVIDER_TLS_KEY_FILE` | empty | PEM private key for built-in HTTPS; must be set with the certificate file |

Boolean values use Go boolean syntax such as `true` and `false`. Invalid or
incomplete configuration aborts startup.

On Unix-like systems, existing profile/debug directories must already be private
(`chmod 700 PATH`); startup fails instead of changing an existing directory's
permissions. Their existing ancestors must not be group/other-writable unless
the writable ancestor has the sticky bit (as standard temporary directories do).
On Windows, these paths and temporary upload snapshots inherit
Windows ACLs, so keep the defaults under your user profile or choose locations
that are accessible only to your account. After upgrading from an older build,
review and manually remove legacy `send-failed-*.png`, `not-ready-*.png`, or
`snapshot-failed-*.png` files if screenshots are now disabled.

`chatgpt-auto` leaves model selection to the ChatGPT page. Other provider model
IDs are passed to the page as model slugs and must also appear in
`CHATGPT_PROVIDER_MODELS`. Availability depends on the current account and UI;
an OpenAI API model ID does not by itself establish a supported web-model slug.
Only add a slug after verifying it for that account. Setting
`CHATGPT_PROVIDER_API_KEY` makes all `/v1/*` requests require
`Authorization: Bearer <key>`. Health endpoints remain unauthenticated but are
still subject to the loopback `Host` restriction by default.

### Upload security model

Uploads are disabled unless both `CHATGPT_UPLOAD_ENABLED=true` and at least one
allowed root are configured. Separate roots with `:` on Unix-like systems and
`;` on Windows. For example:

```sh
# Linux/macOS
CHATGPT_UPLOAD_ALLOWED_ROOTS=/home/me/chatgpt-share:/tmp/chatgpt-export
```

```powershell
# Windows PowerShell
$env:CHATGPT_UPLOAD_ALLOWED_ROOTS = 'C:\Users\me\chatgpt-share;D:\chatgpt-export'
```

Requested paths are canonicalized before containment checks. Filesystem roots
are rejected. A symlink cannot be used to escape an allowed root, and
non-regular files, directly identifiable UNC/device paths,
duplicate filenames, excessive file counts, and files over the configured size
limits are rejected. Supported platforms use a handle-anchored `os.Root` open so
path replacement cannot redirect a validated source outside its allowed root;
uploads fail closed on platforms where Go cannot provide that guarantee. Each
securely opened source is copied into a private, size-limited staging directory;
Chrome receives that immutable snapshot. Cleanup retries after transient browser
file locks, and the next process start removes stale staging directories older
than 24 hours. Do not allow a broad root such as your home directory or filesystem
root. Mapped drives and network-mounted directories are treated as
operator-trusted because they cannot be identified portably; do not configure
one as an allowed root. Copy only the intended files into a small, dedicated
local upload directory and review them before calling the tool.

These controls reduce accidental disclosure; they do not turn upload into a
low-trust capability. A connected agent can upload any qualifying file within an
allowed root without a separate interactive confirmation from this server.
Uploads are rejected when the browser is reached through a non-loopback CDP
endpoint: file inputs interpret path strings on the browser host, so this bridge
cannot prove that a remote Chrome would attach the locally validated bytes.

## OpenCode provider configuration

The checked-in [`opencode.json`](opencode.json) defines the portable custom
provider `chatgpt-browser` with the OpenAI-compatible adapter and the local base
URL `http://127.0.0.1:8787/v1`. It intentionally contains no machine-specific
executable path. Start `chatgpt-mcp provider` before OpenCode, then select
`chatgpt-browser/chatgpt-auto` from OpenCode's model picker (or `/models`).

Use a distinct custom provider ID. Do not rename it to `openai`: clients may
special-case that provider and select the unsupported Responses API. The
configured context and output limits are conservative gateway settings, not
claims about a particular ChatGPT model or subscription. OpenCode needs the
explicit `models` entry even though this bridge also serves `GET /v1/models`.

The local provider does not require an API key by default, but loopback protects
against remote hosts rather than other users or native processes on the same
host. A bearer key is strongly recommended on shared machines. If you set
`CHATGPT_PROVIDER_API_KEY`, add the following property under the provider's
`options` object so OpenCode sends the same value:

```json
"apiKey": "{env:CHATGPT_PROVIDER_API_KEY}"
```

Alternatively, an OpenCode MCP entry can launch `["chatgpt-mcp", "both"]`.
That one process then supplies both MCP tools and the provider address while
keeping all browser work serialized.

## Provider endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/` | Bridge metadata and the `/v1` API location |
| `GET` | `/healthz` | Process and queue health without launching Chrome |
| `GET` | `/readyz` | Alias for lazy process readiness |
| `GET` | `/v1/models` | Configured provider model registry |
| `GET` | `/v1/models/{id}` | One configured model |
| `POST` | `/v1/chat/completions` | Text, tool-call, or response-format completion, buffered or SSE |

With the default unauthenticated loopback configuration (appropriate only when
you trust every account and native process on the host):

```sh
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"chatgpt-auto","messages":[{"role":"user","content":"Say hello"}],"stream":true}'
```

When `CHATGPT_PROVIDER_API_KEY` is set, every `/v1/*` request must also send
`Authorization: Bearer <key>`. Changing the listen address to a non-loopback
interface fails unless `CHATGPT_PROVIDER_ALLOW_REMOTE=true`, a key of at least
32 bytes, and both TLS files are configured. The provider rejects browser-origin
requests in every mode.

Generate a high-entropy key and use a certificate valid for the hostname clients
will connect to. For example, with an existing certificate on Linux or macOS:

```sh
export CHATGPT_PROVIDER_ADDR=0.0.0.0:8787
export CHATGPT_PROVIDER_ALLOW_REMOTE=true
export CHATGPT_PROVIDER_API_KEY="$(openssl rand -base64 32)"
export CHATGPT_PROVIDER_TLS_CERT_FILE=/etc/letsencrypt/live/chatgpt-bridge.example/fullchain.pem
export CHATGPT_PROVIDER_TLS_KEY_FILE=/etc/letsencrypt/live/chatgpt-bridge.example/privkey.pem
./chatgpt-mcp provider
```

The equivalent PowerShell setup is:

```powershell
$env:CHATGPT_PROVIDER_ADDR = '0.0.0.0:8787'
$env:CHATGPT_PROVIDER_ALLOW_REMOTE = 'true'
$keyBytes = New-Object byte[] 32
$keyGenerator = [Security.Cryptography.RandomNumberGenerator]::Create()
$keyGenerator.GetBytes($keyBytes)
$keyGenerator.Dispose()
$env:CHATGPT_PROVIDER_API_KEY = [Convert]::ToBase64String($keyBytes)
$env:CHATGPT_PROVIDER_TLS_CERT_FILE = 'C:\secure\chatgpt-bridge.crt'
$env:CHATGPT_PROVIDER_TLS_KEY_FILE = 'C:\secure\chatgpt-bridge.key'
.\chatgpt-mcp.exe provider
```

Clients must then use `https://<certificate-hostname>:8787/v1` and trust the
certificate issuer. Protect the private-key file with operating-system access
controls. If a TLS reverse proxy runs on the same machine, keep this process on
its default loopback HTTP address and expose only the authenticated HTTPS proxy.

### Provider compatibility and limitations

This is an OpenAI-compatible subset, not an implementation of the complete API:

- `POST /v1/chat/completions` is supported; `/v1/responses` is not. Text
  messages, buffered SSE, and experimental function tool calls are supported.
  Images, audio, remote files, logprobs, and `n` other than `1` are not.
- Every provider completion starts a fresh browser conversation and replays the
  complete request transcript. This preserves stateless Chat Completions
  semantics, but it is slower than a native API and still uses the one shared
  browser worker. It does not create a temporary or non-retained ChatGPT session:
  conversations can remain in normal ChatGPT account history. Unsupported
  request fields, including `store`, are rejected.
- API roles, including `system` and `developer`, are compiled into one clearly
  delimited web-user prompt. The consumer web composer cannot reproduce native
  API role authority exactly. Sampling, token-limit, and stop fields are prompt
  hints rather than native controls.
- SSE sends keepalive comments while ChatGPT is working, then emits the browser
  response as a final content chunk. It is not token-by-token streaming.
- `response_format` accepts `text`, `json_object`, and `json_schema`. JSON output
  is buffered, normalized, and syntax-checked. A schema marked `strict: true` is
  also enforced by the bridge; a violation becomes a provider error rather than
  an invalid successful completion.
- Function tools are prompt-emulated and never executed by this bridge. Tool
  names and the response envelope are validated. Strict function schemas are
  enforced server-side; non-strict argument strings are forwarded so the client
  can apply its own repair, approval, validation, and execution flow. The model
  can still fail to follow this emulated protocol.
- ChatGPT Web exposes no token counts. Compatibility `usage` uses a deliberately
  upper-biased UTF-8 byte estimate for client-side context management, not
  billing. Responses carry `X-ChatGPT-MCP-Usage: estimated`.

## MCP client configuration

OpenCode (`opencode.json`), when `chatgpt-mcp` is on `PATH`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "chatgpt": {
      "type": "local",
      "command": ["chatgpt-mcp", "mcp"],
      "enabled": true
    }
  }
}
```

For another local stdio MCP client, use its equivalent of:

```json
{
  "mcpServers": {
    "chatgpt": {
      "command": "/absolute/path/to/chatgpt-mcp",
      "args": ["mcp"]
    }
  }
}
```

Windows users should point `command` at `chatgpt-mcp.exe`. Environment variables
can normally be supplied through the MCP client's server configuration or the
environment from which the client is launched. Omitting the `mcp` argument is
also supported for backward compatibility.

## Transports and ChatGPT plugins

This repository provides stdio MCP and a separate OpenAI-compatible provider
that defaults to loopback HTTP and can be configured for HTTPS. The provider
endpoint is not MCP, and neither transport is a ChatGPT plugin endpoint.

ChatGPT does not connect directly to an arbitrary local stdio process. OpenAI's
[connection guidance](https://developers.openai.com/plugins/deploy/connect-chatgpt)
requires either a public HTTPS Streamable HTTP MCP endpoint or, for private
development, Secure MCP Tunnel connected to the stdio/HTTP server. This repository
does not provide Streamable HTTP MCP or a turnkey public deployment. Its optional
TLS termination applies only to the OpenAI-compatible provider; it does not turn
that endpoint into MCP or a ChatGPT plugin. Because the bridge controls an
authenticated browser and may access explicitly allowed files, do not expose
either transport merely to make it remotely reachable.

## Models

`chatgpt_ask` accepts an optional `model` string. Available identifiers and the
page's selection behavior depend on the ChatGPT account and may change without
notice. A model query parameter can silently fall back, so the server treats model
selection as unverified until the page confirms it. Do not use the returned model
field as a billing, entitlement, or reproducibility guarantee.

## Reliability and known limits

- Browser selectors and page state are private implementation details of
  `chatgpt.com`; they can change at any time.
- Completion tracking requires a new assistant turn after the prompt and observes
  stable explicit terminal state. This is safer than returning whichever text was
  already on the page, but still depends on the current DOM.
- A fresh chat is bound to the outgoing user-message identity observed for the
  send request before its new `/c/...` route is accepted. Replies remain bound to
  the tracked conversation; use `chatgpt_new_chat` after manual navigation or a
  quarantined/failed operation.
- Extracted assistant content is converted to Markdown while preserving paragraphs,
  headings, lists, links, code blocks, and tables. Only known answer-content nodes
  are read; unknown DOM shapes fail closed rather than returning an entire turn.
  Interactive widgets, generated files, images, citations, and other rich UI
  may not round-trip through the text result.
- Recognized deep-research/research UI workflows are rejected rather than polled:
  the private web UI exposes no durable completion contract, so accepting a quiet
  intermediate phase could return an incomplete result. Detection still depends
  on explicit DOM state and can break when the site changes. Local cancellation
  stops waiting, but cannot guarantee that server-side generation in ChatGPT was
  canceled. Use the official
  [Deep Research API](https://developers.openai.com/api/docs/guides/deep-research)
  when reliability and automation support matter.
- The browser profile, conversation, and selected model belong to one server
  process. Calls are serialized rather than parallelized.
- Rod's `leakless` launcher helper is enabled so a launched browser is cleaned
  up if the bridge exits unexpectedly. Endpoint security software must allow
  that helper to run; a forced system-level termination can still leave Chrome
  running, so check for an orphan before restarting against the same profile.
- If a call fails or is canceled after changing the composer, the browser is
  quarantined so a queued request cannot inherit a draft, attachment, or active
  generation. Call `chatgpt_new_chat` to establish a verified clean state.
- CI uses unit tests, browser-backed DOM fixtures, and local MCP
  integration/concurrency tests. It cannot perform
  an authenticated end-to-end ChatGPT test, so a green build does not prove that
  the current website still works.

Failure screenshots under `CHATGPT_DEBUG_DIR` are the best starting point when a
selector no longer matches.

## Development

Run the same checks as CI:

```sh
go mod verify
gofmt -w .
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
```

`gofmt -w .` modifies files; use `gofmt -l .` for a read-only check.

## License

MIT. See [LICENSE](LICENSE).
