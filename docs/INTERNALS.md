# kirogo internals

How kirogo works, what the Kiro API actually does versus what its schema claims, and why
certain decisions went the way they did. Read this before changing anything on the wire.

- [Architecture](#architecture)
- [Undocumented API behaviour](#undocumented-api-behaviour)
- [Credentials](#credentials)
- [Model resolution](#model-resolution)
- [Token accounting](#token-accounting)
- [Output limits](#output-limits)
- [Design decisions](#design-decisions)
- [Full configuration](#full-configuration)

## Architecture

```
┌──────────────┐        ┌────────────┐        ┌─────────────────┐
│  Cursor      │        │            │        │                 │
│  Claude Code │◄──────►│   kirogo   │◄──────►│   Kiro backend  │
│  Cline / Roo │  HTTP  │  :8000     │  AWS   │  CodeWhisperer  │
│  Zed / Aider │        │            │        │                 │
└──────────────┘        └────────────┘        └─────────────────┘
  OpenAI or                                     vnd.amazon.
  Anthropic JSON                                eventstream
```

```
cmd/kirogo/          entry point and CLI
internal/config/     configuration and precedence
internal/auth/       credentials, discovery, both refresh flows, regions
internal/kiro/       wire protocol: requests, event stream decoder, errors
internal/catalog/    model discovery, name resolution, effort schemas
internal/translate/  OpenAI and Anthropic → Kiro, plus the structural rules
internal/api/        HTTP surfaces, streaming, usage accounting
internal/store/      read-only SQLite reader for kiro-cli databases
internal/util/       ids, redaction, token estimation
```

Both API surfaces translate into one intermediate form, then into one Kiro request. Both
consume the same upstream event stream through the same collector. That is deliberate:
the streaming and non-streaming paths cannot drift apart, because they share the code
that decides what a response contains.

## Undocumented API behaviour

kirogo is built against observed behaviour, verified against the Kiro IDE bundle and
confirmed against the live service. Several things differ from what the schema alone
suggests.

### The catalog lives on a different host

The chat operation is on `runtime.{region}.kiro.dev`, but `ListAvailableModels` belongs
to a separate service on `management.{region}.kiro.dev`. The runtime host returns 404 for
catalog paths.

They also need different headers. The runtime call requires `x-amz-target` and
`application/x-amz-json-1.0`; the REST catalog call must send neither. Sending the
runtime header set to the catalog endpoint is one way to produce that 404.

### `systemPrompt` is declared but does not work

The schema declares a top-level `systemPrompt` field. The deployed backend answers
`Improperly formed request.` (`REQUEST_BODY_INVALID`) for any request carrying it, and
**silently ignores** a copy nested inside `conversationState` — the model simply disobeys
it, with no error to explain why.

All four placements were probed. Folding the system prompt into the first user turn is
the only one the live service honours, so that is the default. Set
`KIRO_SYSTEM_PROMPT_MODE=field` to use the declared field if the deployment ever catches
up with its own schema.

### Reasoning in history is a union

Not a bare `{text, signature}`. The wire shape is a union with two members:

```json
"reasoningContent": { "reasoningText": { "text": "...", "signature": "..." } }
```

```json
"reasoningContent": { "redactedContent": "<base64>" }
```

Reasoning is dropped rather than sent when the signature is missing, and also when it
came from a different model than the current request. The backend rejects a signature it
cannot verify with `THINKING_SIGNATURE_INVALID`; kirogo retries once with reasoning
stripped from history, guarded by a flag so it can never loop.

### Effort nests under a per-model key

Reasoning effort goes in `additionalModelRequestFields`, but under a key that differs by
model — either `output_config` or `reasoning`:

```json
{ "output_config": { "effort": "max" } }
{ "reasoning":     { "effort": "max" } }
```

kirogo probes each model's own `additionalModelRequestFieldsSchema` to find which key and
which levels that model accepts, rather than assuming. Assuming is wrong for at least one
shipping model. Valid levels are `low`, `medium`, `high`, `xhigh` and `max`; some models
also advertise `none`, which is filtered out of the `:level` variants but still reachable
by passing `reasoning_effort: "none"` explicitly.

### The response is a real binary event stream

The body is genuine `vnd.amazon.eventstream`: length-prefixed frames with CRC-checked
preludes and payloads, typed headers, and events split across TCP boundaries at arbitrary
points. kirogo decodes the framing properly rather than scanning for JSON, which is what
stops a chunk boundary inside a string from corrupting output.

The union declares 19 event types. In practice the service sends
`assistantResponseEvent`, `reasoningContentEvent`, `toolUseEvent`, `contextUsageEvent` and
`meteringEvent`. `metadataEvent`, which carries exact token counts, is declared but rarely
sent — which is why usage is often flagged as estimated.

`metadataEvent` also carries `stopReason` and `stopDetails` at runtime even though the
schema declares only `tokenUsage`, so it is parsed defensively.

### "Improperly formed request." means almost nothing

The backend returns that one message for every kind of schema violation, with no further
detail. kirogo works around the known causes:

- empty message content
- non-alternating roles, or a history not starting with a user turn
- tool results with no matching tool call
- `additionalProperties` anywhere in a tool schema
- empty `required` arrays
- blank tool descriptions
- over-long tool descriptions and names

If you still hit it, run with `LOG_LEVEL=DEBUG` to capture the exact payload sent
upstream. That is the only practical way to narrow it down.

## Credentials

Sources are tried in order, first usable one wins:

1. `KIRO_CREDS_FILE`
2. `REFRESH_TOKEN`
3. auto-discovery in `~/.aws/sso/cache`
4. `KIRO_CLI_DB_FILE`

A `KIRO_CREDS_FILE` that is set but unusable is a hard error, not a silent fall-through,
so a typo in the path is reported instead of hidden.

Two refresh flows are supported and detected automatically: credentials carrying
`clientId` and `clientSecret` use AWS SSO OIDC, everything else uses the Kiro desktop
flow. Refresh happens 600 seconds before expiry, single-flighted so concurrent requests
trigger one refresh rather than a stampede. A rotated token is written back to a file
source, which is why pointing `KIRO_CREDS_FILE` at your real token file is better than
copying the token into `.env`.

`TokenType` is an HTTP header, not a body field: `external_idp` → `EXTERNAL_IDP`,
`machine_token` → `KIRO_MACHINE_TOKEN`, `api_key` → `API_KEY`, `IdC` → `SSO_OIDC`.

kiro-cli databases are read, never written, by a small read-only SQLite reader built into
`internal/store` — that reader exists so kirogo can support kiro-cli without taking a
dependency. It reads the whole file, parses column positions out of the `CREATE TABLE`
statement rather than assuming an order, and refuses a database in WAL mode rather than
reading it, because the main file can hold a token the write-ahead log has already
replaced.

## Model resolution

Client names are normalised before use:

```
claude-haiku-4-5-20251001  →  claude-haiku-4.5     strip date suffix
claude-sonnet-4-20250514   →  claude-sonnet-4
claude-3-7-sonnet          →  claude-3.7-sonnet    dash to dot, reorder
claude-4.5-opus-high       →  claude-opus-4.5      inverted name, effort high
claude-sonnet-4-5[1m]      →  claude-sonnet-4.5    strip context marker
sonnet-4.5                 →  claude-sonnet-4.5    add vendor prefix
```

Resolution builds a candidate list rather than transforming in place. An effort suffix is
split off first, but the full unsplit name stays a candidate, so `claude-4.5-opus-high`
can normalise to `claude-opus-4.5` with `effort=high` while a model whose id genuinely
ends in `-low` still wins over the effort reading.

An exact id match short-circuits before any fuzzy matching. A name that cannot be placed
is forwarded upstream unchanged and logged at INFO — the backend is the authority on which
models exist, so a model newer than your kirogo build still works. Kiro's own selector is
advertised as `auto-kiro`, because Cursor ships a model called `auto` and the names
collide.

## Token accounting

Exact counts are passed through untouched when present:

```
prompt_tokens     = uncachedInputTokens + cacheReadInputTokens + cacheWriteInputTokens
completion_tokens = outputTokens
```

Without them, the total is derived from the reported context-usage percentage against the
model's context window. Only if that is missing too does the local estimator get
involved. Anything not measured is flagged `"estimated": true`.

Responses also carry `cache_read_input_tokens`, `cache_write_input_tokens`,
`context_usage_percentage` and `credits_used`. These are additive; strict clients ignore
them.

### Why the estimate is approximate

`count_tokens` answers locally with no upstream call, which is what makes it fast enough
for Claude Code to call before every request.

Exact `cl100k_base` parity is not reachable in pure Go: its pre-tokenizer needs a negative
lookahead, `\s+(?!\S)`, which Go's RE2 engine does not support. Shipping a BPE vocabulary
to chase parity would add megabytes for an advisory number.

Two measured effects pull in opposite directions:

| | |
|---|---|
| Backend hidden prompt | ~4,100 tokens no client can see, dragging the estimate **low** |
| Estimator ratio | ~3.5 chars/token against a real ~5, pushing it **high** |

The over-count is deliberate: under-reporting would let a client believe a request fits
when it does not. On short requests the hidden prompt dominates; on long ones the
over-count largely cancels it. Measured on a live account:

```
prompt size      local estimate    real upstream    gap
1 word                      11            4,139    4,128
~250 tokens                396            4,419    4,023
~2,500 tokens            3,863            6,607    2,744
```

## Output limits

`max_tokens` is enforced locally. The Kiro backend has no max-tokens parameter, so the
ceiling cannot be handed upstream. Ignoring it was not an option either: `max_tokens` is
a required field of the Anthropic API and clients size their context against it.

kirogo cuts output at the ceiling and reports `finish_reason: "length"` (OpenAI) or
`stop_reason: "max_tokens"` (Anthropic). The budget and the usage report share the same
arithmetic, so the number reported can never exceed the number requested. Cuts land on
rune boundaries, so multi-byte text is never split into invalid UTF-8.

**Reasoning shares the ceiling**, since the backend bills it as output. With extended
thinking, set `max_tokens` well above the thinking budget or reasoning will consume the
whole allowance and leave no text. Anthropic's own API behaves the same way.

**Tool calls are exempt.** Tool arguments are machine-directed JSON, and half of one is a
fragment no client can parse. A tool call is always delivered whole, and `stop_reason`
reports `tool_use` rather than the ceiling so an agent runs the call instead of stopping.

A request that offered tools is drained to the end of the stream rather than released
early, because a tool call may still be on its way when the ceiling is reached. Checking
whether a call has already started is not enough — the first fragment may not have
arrived yet. With no tools offered, the connection is released immediately, since nothing
further can reach the client.

Because the ceiling is applied on the way out, the backend has already generated and
billed the full response. Enforcement corrects what the client sees; it does not save
credits.

## Design decisions

**Requests are never trimmed.** A conversation over `KIRO_MAX_PAYLOAD_BYTES` is refused
with a 413 that explains why. Deciding which messages matter is the client's job;
silently deleting history produces baffling model behaviour. The backend starts rejecting
somewhere near 615,000 bytes with an unhelpful error, so kirogo stops short and says why.

**Upstream truncation is detected, not patched.** When the backend cuts a response short
it stops sending token accounting. kirogo notices, logs it as an upstream limit, and
reports the length stop reason. Nothing synthetic is injected. A response kirogo cut
itself to honour `max_tokens` is reported the same way to the client but never logged as
an upstream fault, because it is not one.

**A stalled request is retried, a started one is not.** If the model produces nothing
within `FIRST_TOKEN_TIMEOUT`, kirogo abandons the attempt and re-issues it. Once a single
byte has reached the client, no retry happens — a partial response is never restarted.

**Streaming connections are not pooled.** Each stream gets a fresh connection and
`Connection: close`, because reusing a pooled connection for a long-lived stream leaks
sockets in `CLOSE_WAIT` when a network interface changes.

**Long tool descriptions are relocated, not dropped.** Descriptions over
`TOOL_DESCRIPTION_MAX_LENGTH` move into the system prompt with a pointer left behind,
because Claude Code and Cline ship very long tool docs that the backend rejects inline.

**Message merging runs before the tool rules.** Checking for orphaned tool results first
would see only one of two consecutive user turns as adjacent to the assistant turn, and
would wrongly inline the second turn's results. This ordering differs from the Python
reference for that reason.

**Secrets stay out of logs.** Tokens are redacted at every log level including `DEBUG`.
The `Authorization` header logs as `Bearer <redacted>`.

## Full configuration

Precedence: command-line flags → real environment variables → `.env` → defaults.
Flags: `-host`, `-port`, `-dump-models`, `-version`.

| Variable | Default | Purpose |
|---|---|---|
| `PROXY_API_KEY` | `kirogo` | Client key. Warns loudly if left at the default. |
| `SERVER_HOST` | `127.0.0.1` | Listen address. |
| `SERVER_PORT` | `8000` | Listen port. |
| `KIRO_CREDS_FILE` | — | Credentials JSON path. `~` is expanded. |
| `REFRESH_TOKEN` | — | Refresh token on its own. |
| `KIRO_CLI_DB_FILE` | — | kiro-cli SQLite database, read-only. |
| `PROFILE_ARN` | — | Profile ARN, when not in your credentials. |
| `KIRO_REGION` | `us-east-1` | Token refresh region. |
| `KIRO_API_REGION` | — | Override the detected API region. |
| `KIRO_EFFORT_LEVEL` | — | Default reasoning effort. |
| `KIRO_EXPOSE_EFFORT_VARIANTS` | `true` | Advertise `model:level` ids. |
| `KIRO_AGENT_MODE` | `vibe` | `x-amzn-kiro-agent-mode` header. |
| `KIRO_VERSION` | `0.7.45` | Version reported in the User-Agent. |
| `KIRO_SYSTEM_PROMPT_MODE` | `inline` | System prompt placement. |
| `KIRO_MODEL_REFRESH_TTL` | `3600` | Catalog freshness, seconds. |
| `FIRST_TOKEN_TIMEOUT` | `15` | Seconds to wait for the first token. |
| `FIRST_TOKEN_MAX_RETRIES` | `3` | First-token attempts before a 504. |
| `STREAMING_READ_TIMEOUT` | `300` | Maximum gap between chunks. |
| `TOOL_DESCRIPTION_MAX_LENGTH` | `10000` | Relocation threshold. `0` disables. |
| `KIRO_MAX_PAYLOAD_BYTES` | `600000` | Refuse larger requests with a 413. |
| `LOG_LEVEL` | `INFO` | `DEBUG` logs the exact upstream payload. |

Standard `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are honoured. No SOCKS5 support; run
a local HTTP bridge if you need it.

Variables that other Kiro proxies define and kirogo does not implement
(`FAKE_REASONING*`, `ACCOUNT_*`, `TRUNCATION_RECOVERY`, `AUTO_TRIM_PAYLOAD`,
`WEB_SEARCH_ENABLED`, `DEBUG_MODE`, `VPN_PROXY_URL` and friends) are accepted and ignored
with one log line each, so an existing `.env` still boots.
