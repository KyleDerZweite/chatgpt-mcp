# Multi-tab concurrency design

## Status and scope

This document describes the intended concurrency model for one `chatgpt-mcp`
process controlling several ChatGPT tabs through one Chrome/Brave CDP
connection. It is a design contract, not a statement that concurrency is
currently implemented.

Today the bridge is deliberately single-threaded:

- `browser.Session` owns one mutable `Page`.
- `chatgpt.Client` stores one global model, chat ID, and operation gate.
- MCP and provider calls are serialized through that gate.
- Canceling tab creation can abort the whole CDP transport.
- `Session.Invalidate` discards the entire browser connection.
- Provider admission accepts up to eight calls, but they wait behind the one
  browser gate.

The transport-abort and global-invalidation behavior must be removed before
the active-operation limit is raised above one. Otherwise one canceled or
dirty request could break every in-flight conversation.

## Target architecture

```text
BrowserManager (one browser/CDP connection)
|-- Scheduler (bounded active browser operations)
|-- ConversationRegistry
|   |-- chat ID A -> Conversation -> Tab + per-chat gate
|   `-- chat ID B -> Conversation -> Tab + per-chat gate
`-- provider request -> ephemeral Tab, closed after completion
```

`BrowserManager` owns connection establishment, reconnection, target creation,
and shutdown. A `Tab` wraps one bridge-owned page/target and provides
page-specific navigation, origin verification, readiness, screenshots, and
close/invalidate operations. No package should rely on a globally selected
page.

`ConversationRegistry` maps actual ChatGPT conversation IDs to persistent MCP
conversation entries. Each entry owns its tab, model metadata, last-used time,
dirty state, reference count, and a context-aware capacity-one operation gate.
A global scheduler limits operations across distinct tabs.

Recommended type boundaries:

```go
type BrowserManager interface {
    OpenTab(context.Context, string) (Tab, error)
    Close(context.Context) error
}

type Tab interface {
    Navigate(context.Context, string) error
    VerifyOrigin(context.Context) error
    WaitReady(context.Context, time.Duration) error
    Close(context.Context) error
}
```

ChatGPT DOM helpers should accept a `Tab` or page interface. This makes the
registry and scheduler testable without a live browser.

## Chat-ID semantics

Persistent tabs are keyed by the ChatGPT ID from a verified `/c/<id>` URL, not
by title, model, MCP request ID, or an OpenAI completion ID.

A new conversation has no ChatGPT ID until its first message is accepted. It
therefore starts as a private provisional entry. As soon as the page moves to
`/c/<id>`, the client atomically promotes the entry into the chat-ID map. The
ID should be captured after submission and during polling, even if the final
response times out. An error result should include a discovered ID so the
caller can recover the conversation.

Promotion must detect an unexpected collision with an existing entry and must
not silently replace either tab. Supplied IDs require an anchored conservative
validator, followed by exact origin and final-path verification after
navigation.

An explicit, valid ID absent from the registry can be opened lazily at
`https://chatgpt.com/c/<id>`. This supports replies after tab eviction or
process restart. Redirects to login, home, another conversation, or another
origin are errors.

MCP changes:

- `chatgpt_ask` continues to create a fresh conversation and returns
  `chat_id`.
- `chatgpt_reply` and `chatgpt_upload` gain an optional `chat_id`.
- Omitting it uses a protected legacy-default entry for compatibility, but
  explicit IDs are required for reliable concurrent workflows.
- `chatgpt_close_chat(chat_id)` closes the local tab only; it does not delete
  ChatGPT history.
- `chatgpt_list_chats` reports open IDs, state, model, and idle time.
- `chatgpt_new_chat` may keep a provisional legacy-default entry until its
  first reply is promoted.

Model and dirty-page state belong to a conversation entry. The global `model`
and `chatID` fields should disappear; each provider request instead owns the
state of its ephemeral tab.

## Locking and lifecycle rules

Use this ordering consistently:

1. Hold the registry mutex only for map, reference, state, and LRU updates.
   Never perform CDP or other blocking I/O while holding it.
2. Take a registry lease so the entry cannot be evicted.
3. Acquire the per-conversation gate.
4. Acquire a global scheduler slot.
5. Operate on that entry's tab.
6. Release the scheduler slot, conversation gate, and lease in that order.

Same-chat operations are serialized. Different chat IDs may overlap up to the
global limit. Waiting for a busy chat must not consume a global slot.

Tab allocation first evicts the least-recently-used idle tab when the open-tab
limit is reached. Busy, leased, provisional, or dirty-cleanup tabs are never
evicted. An evicted ID can be lazily reopened later.

The browser connection has a monotonically increasing epoch. Tabs record the
epoch in which they were created. A genuine transport loss advances the epoch
and makes every old handle stale; later ID-addressed operations reconnect once
and reopen their conversation. A tab-local DOM or composer failure must not
invalidate the shared connection.

In attach mode, the manager closes only targets it created. It must never send
`Browser.close` to an externally owned browser.

## Cancellation and ambiguous sends

Request cancellation affects only the selected tab:

- Cancellation while waiting for a chat gate or scheduler slot returns
  without leaking either resource.
- Cancellation during target creation must not close the shared CDP
  transport. Finish/drain the bounded creation attempt and close any target
  produced after cancellation.
- Cancellation during generation clicks Stop only in that tab.
- Keep the conversation gate until Stop is confirmed, or until a reload makes
  the composer clean.
- If cleanup cannot be confirmed, close the tab, retain a known chat ID, and
  reopen it lazily on the next call.

Never automatically retry after the send action: delivery is ambiguous and a
retry could duplicate a user message. Connection-loss recovery applies to a
later request, not to replaying the failed request.

On process shutdown, stop admission, cancel active operation contexts, wait
for cleanup with a bound, close owned tabs, and finally close or detach the
browser connection according to ownership.

## Provider behavior

`POST /v1/chat/completions` remains stateless. Each request receives a fresh,
ephemeral tab, replays the complete API transcript, and closes the tab on
success, error, or cancellation. Provider tabs use the global scheduler but do
not enter the persistent chat registry.

Reusing a ChatGPT chat-ID tab for ordinary provider calls is incorrect: the
request already includes full history, so reuse would duplicate system,
assistant, and tool messages. A response may expose
`X-ChatGPT-Conversation-ID` for diagnostics, but the ID must not imply sticky
OpenAI-compatible session state.

Provider admission and browser execution should be separate measurements:

- admission bounds active plus queued HTTP requests and returns `429` when
  full;
- the scheduler bounds actual browser operations across provider and MCP;
- health output reports `active`, `queued`, `open_tabs`, `max_active`, and CDP
  connection state.

Function-tool loops remain stateless across API calls: OpenCode returns tool
results in the next full transcript, and the bridge compiles that transcript
into a new web prompt.

## Configuration defaults

Start conservatively:

| Variable | Default | Meaning |
|---|---:|---|
| `CHATGPT_MAX_CONCURRENT_TABS` | `2` | Maximum simultaneous browser operations |
| `CHATGPT_MAX_OPEN_TABS` | `6` | Persistent plus ephemeral bridge-owned tabs |
| `CHATGPT_TAB_IDLE_MINUTES` | `30` | Idle duration before a persistent tab is eligible for eviction |
| `CHATGPT_PROVIDER_QUEUE_SIZE` | `8` | Maximum admitted provider calls, including active calls |

Require `MAX_OPEN_TABS >= MAX_CONCURRENT_TABS`, validate overflow and sensible
hard caps, and retain `1` as a documented fallback for accounts or machines
that do not tolerate parallel generation. Parallel web generations may
increase renderer memory, account throttling, and automation-detection risk.

## Cross-process direction

Only one Chrome/Brave process may launch against a user-data directory.
Multiple WebSocket clients can attach to one CDP endpoint, but independent
bridge processes do not share chat locks, tab ownership, admission limits, or
account-level throttling. Two processes can therefore mutate the same ChatGPT
conversation concurrently even if they use separate tabs.

The initial supported policy should be one bridge controller per canonical
profile or CDP endpoint. Acquire an OS/file lease and fail clearly when another
controller owns it. Shared-CDP attachment, if retained as an expert override,
must be documented as uncoordinated and unsafe for the same chat ID. The CDP
endpoint must remain loopback-only because it grants access to the authenticated
browser.

The long-term multi-client solution is one daemon that owns Brave, the tab
registry, and scheduling. It should expose both `/v1` and a local Streamable
HTTP MCP endpoint. OpenCode, Codex, and other clients then share the daemon
instead of spawning independent stdio processes. A thin stdio-to-daemon proxy
can preserve compatibility for clients without HTTP MCP support.

## Phased implementation

1. **Introduce browser manager and tab interfaces.** Keep the scheduler limit
   at one. Move every page operation onto a tab, add connection epochs, and
   remove per-request transport aborts and global tab invalidation.
2. **Add the conversation registry.** Implement provisional promotion, lazy
   reopen, per-chat metadata, explicit MCP chat IDs, close/list operations, and
   legacy-default behavior.
3. **Add bounded scheduling.** Implement per-chat gates, global slots, open-tab
   limits, LRU idle eviction, provider ephemeral tabs, and accurate health
   metrics. Only then raise the default active limit above one.
4. **Harden recovery.** Add tab-local Stop/dirty cleanup, login coordination,
   browser-epoch loss handling, concurrent screenshot rotation, and bounded
   shutdown draining.
5. **Enforce process ownership.** Add the profile/CDP controller lease. Follow
   with a single-daemon HTTP MCP transport if multiple applications must share
   one authenticated browser.

## Test gates

Concurrency must remain disabled until automated tests prove all of the
following:

- Same-chat calls serialize while distinct chats overlap.
- The global active limit and open-tab limit are never exceeded.
- Canceled gate, scheduler, and tab-creation waiters leak no resources.
- Canceling or invalidating one tab does not affect another.
- Provider tabs always close and never change MCP default state.
- Provisional promotion, collision handling, and timeout-with-discovered-ID
  behave deterministically.
- Unknown IDs reopen lazily and redirects are verified exactly.
- LRU eviction never chooses active or leased entries.
- Upload rollback invalidates only its conversation tab.
- Transport loss advances one epoch and permits later recovery without
  resubmitting an ambiguous message.
- Shutdown drains all work and leaves an externally owned browser running.
- `go test -race ./...` passes in CI under simultaneous registry lookup,
  promotion, eviction, provider, and MCP workloads.

Before release, run a manual authenticated E2E exercise with two distinct
conversations generating concurrently, two simultaneous replies targeting one
ID, cancellation isolation, an upload, an OpenCode tool-call loop, idle
eviction, and process-restart recovery by chat ID.
