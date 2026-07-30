<div align="center">

<h1>kirogo</h1>

<h3>Use your Kiro subscription from any AI coding tool</h3>

<p>
A self-hosted proxy that exposes Kiro (Amazon Q Developer / AWS CodeWhisperer) models<br>
through <b>OpenAI</b>- and <b>Anthropic</b>-compatible APIs — so Cursor, Claude Code,<br>
Cline, Continue, Zed and Aider all just work. One static Go binary, zero dependencies.
</p>

[![CI](https://img.shields.io/github/actions/workflow/status/jasminnanda/kirogo/ci.yml?branch=main&style=for-the-badge&label=CI)](https://github.com/jasminnanda/kirogo/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg?style=for-the-badge)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev)
[![Dependencies](https://img.shields.io/badge/dependencies-0-success?style=for-the-badge)](go.mod)
[![Tests](https://img.shields.io/badge/tests-1082%20passing-brightgreen?style=for-the-badge)](#-development)

<a href="#-quick-start"><b>Quick start</b></a> ·
<a href="#-models"><b>Models</b></a> ·
<a href="#-connect-your-editor"><b>Clients</b></a> ·
<a href="#-reasoning-effort"><b>Reasoning</b></a> ·
<a href="#-configuration"><b>Config</b></a> ·
<a href="docs/INTERNALS.md"><b>Internals</b></a>

<br>

```
   Cursor ─┐                                        ┌─ Claude Opus 5
Claude Code├─┐                                    ┌─┤  GPT 5.6
    Cline ─┘ │   OpenAI / Anthropic JSON           │ │  Qwen3 Coder
             ├──────────►  kirogo :8000  ──────────┤ │  GLM 5
      Zed ─┐ │                          vnd.amazon │ │  DeepSeek
    Aider ─┴─┘                        .eventstream └─┤  MiniMax
                                                     └─ + whatever your plan adds
```

</div>

---

Your Kiro subscription already includes Claude, GPT, Qwen, GLM, DeepSeek and MiniMax
models — but only inside Kiro's own editor. kirogo puts that same catalog behind the two
API shapes every other tool already speaks, reading the credentials Kiro IDE or `kiro-cli`
already wrote on your machine. Nothing leaves your machine except the calls you were
already making.

## ✨ Features

| | |
|:--|:--|
| 🔌 **Two APIs** | OpenAI `/v1/chat/completions` and Anthropic `/v1/messages`, streaming and not |
| 📦 **Zero dependencies** | Go standard library only. `go.mod` has no `require` block |
| ⚡ **One static binary** | 7.5 MB, no runtime, no interpreter, no Docker |
| 🛠️ **Full tool calling** | Round trips, parallel calls, streaming argument deltas, both APIs |
| 🧠 **Native reasoning** | Real thinking blocks with signature replay, not simulated |
| 👁️ **Vision** | Image input on both APIs |
| 🎚️ **Effort in the model name** | `claude-opus-5:max` — the only way most clients can pick effort |
| 🔄 **Live catalog** | Models come from *your* account, so a new model works the day you get it |
| ✂️ **`max_tokens` enforced** | The backend has no such parameter, so kirogo applies the ceiling itself |
| 🔐 **Zero-config auth** | Finds your Kiro login automatically. Reads kiro-cli's SQLite read-only |

## 🤖 Models

Whatever your plan includes — nothing is hardcoded. A typical account today:

| Model | Context | Rate | Reasoning effort |
|:--|--:|--:|:--|
| `claude-opus-5` | 1M | 2.2x | low → max, default high |
| `claude-opus-4.8` | 1M | 2.2x | low → max, default high |
| `claude-opus-4.7` | 1M | 2.2x | low → max, default xhigh |
| `claude-opus-4.6` | 1M | 2.2x | low → max, default high |
| `claude-opus-4.5` | 200K | 2.2x | — |
| `claude-sonnet-5` | 1M | 1.3x | low → max, default high |
| `claude-sonnet-4.6` | 1M | 1.3x | low → max, default high |
| `claude-sonnet-4.5` | 200K | 1.3x | — |
| `claude-sonnet-4` | 200K | 1.3x | — |
| `claude-haiku-4.5` | 200K | 0.4x | — |
| `gpt-5.6-sol` | 272K | 2.4x | none → max, default high |
| `gpt-5.6-terra` | 272K | 1.2x | none → max, default high |
| `gpt-5.6-luna` | 272K | 0.6x | none → max, default high |
| `qwen3-coder-next` | 256K | **0.05x** | — |
| `glm-5` | 200K | 0.5x | — |
| `deepseek-3.2` | 164K | 0.25x | — |
| `minimax-m2.5` | 196K | 0.25x | — |
| `minimax-m2.1` | 196K | 0.15x | — |
| `auto` | 1M | 1x | — |

> [!TIP]
> Run `./kirogo -dump-models` for your own live list. `auto` is advertised as `auto-kiro`,
> because Cursor ships its own model called `auto` and the names collide.

## 🚀 Quick start

**1** · Install [Kiro](https://kiro.dev), sign in once, close it. That writes a token
kirogo finds on its own.

**2** · Install it, with Go 1.24 or newer:

```sh
go install github.com/jasminnanda/kirogo/cmd/kirogo@latest
```

<details>
<summary>No Go toolchain, or prefer building from source?</summary>
<br>

Grab a prebuilt binary from [Releases](https://github.com/jasminnanda/kirogo/releases) —
Linux, macOS and Windows on both amd64 and arm64, static, nothing to install.

Or build it yourself:

```sh
git clone https://github.com/jasminnanda/kirogo.git && cd kirogo
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o kirogo ./cmd/kirogo
```

Cross-compiling is the usual one-liner, and CI builds all five targets on every push:

```sh
GOOS=darwin GOARCH=arm64 go build -o kirogo-macos-arm64 ./cmd/kirogo
GOOS=windows GOARCH=amd64 go build -o kirogo.exe ./cmd/kirogo
```

</details>

**3** · Run it:

```sh
PROXY_API_KEY=pick-a-long-random-string kirogo
```

```console
INFO credentials loaded source="auto-discovered credentials file" flow=kiro-desktop
INFO model catalog loaded models=19 default_model=auto
INFO kirogo listening addr=127.0.0.1:8000 models=19
```

**4** · Point your editor at `http://127.0.0.1:8000` and you're done.

> [!IMPORTANT]
> kirogo binds to localhost because it holds a live AWS token behind one shared key.
> `SERVER_HOST=0.0.0.0` exposes that to your whole network. Use a long random
> `PROXY_API_KEY` — anyone who reaches the port can spend your quota.

## 🔌 Connect your editor

<details>
<summary><b>Cursor</b></summary>
<br>

Settings → Models → add an OpenAI-compatible provider.

- **Base URL** · `http://127.0.0.1:8000/v1`
- **API key** · your `PROXY_API_KEY`

Add model names by hand with the `:level` suffix, e.g. `claude-opus-5:max`. Use
`auto-kiro` rather than `auto`.

</details>

<details>
<summary><b>Claude Code</b></summary>
<br>

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8000
export ANTHROPIC_AUTH_TOKEN=your-proxy-api-key
export ANTHROPIC_MODEL=claude-opus-5
claude
```

Uses the Anthropic surface end to end: `count_tokens`, thinking blocks with signature
replay, and `input_json_delta` tool streaming.

</details>

<details>
<summary><b>Cline / Roo Code</b></summary>
<br>

Choose the **OpenAI Compatible** provider.

- **Base URL** · `http://127.0.0.1:8000/v1`
- **API key** · your `PROXY_API_KEY`
- **Model** · `claude-opus-5:xhigh`

</details>

<details>
<summary><b>Continue</b></summary>
<br>

```yaml
models:
  - name: Kiro Opus 5
    provider: openai
    model: claude-opus-5:xhigh
    apiBase: http://127.0.0.1:8000/v1
    apiKey: your-proxy-api-key
```

</details>

<details>
<summary><b>Zed</b></summary>
<br>

```json
{
  "language_models": {
    "openai": {
      "api_url": "http://127.0.0.1:8000/v1",
      "available_models": [{ "name": "claude-opus-5:max", "max_tokens": 1000000 }]
    }
  }
}
```

</details>

<details>
<summary><b>Aider</b></summary>
<br>

```sh
export OPENAI_API_BASE=http://127.0.0.1:8000/v1
export OPENAI_API_KEY=your-proxy-api-key
aider --model openai/claude-opus-5:xhigh
```

</details>

<details>
<summary><b>curl</b></summary>
<br>

```sh
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"claude-opus-5:max","messages":[{"role":"user","content":"hi"}]}'
```

</details>

## 🧠 Reasoning effort

Kiro uses named effort levels, not token budgets. Three ways to pick one, highest
priority first:

```sh
claude-opus-5:max          # in the model name — works with any client
reasoning_effort: "max"    # OpenAI requests
thinking.budget_tokens     # Anthropic requests, bucketed to a level
KIRO_EFFORT_LEVEL=max      # default for everything
```

`/v1/models` advertises one `model:level` id per supported level, so they show up in your
editor's model picker. A level a model doesn't offer is clamped to that model's default
rather than rejected.

The `gpt-5.6-*` models also offer `none`, which turns reasoning off — use
`gpt-5.6-sol:none` or `reasoning_effort: "none"`. On a model that has no `none`, a request
for no reasoning gets the lowest level it does offer, because omitting the field entirely
would hand the choice back to the backend, whose default is `high`.

## 📡 Endpoints

| | Path | |
|:--|:--|:--|
| `GET` | `/` · `/health` | Liveness and status. Unauthenticated. |
| `GET` | `/v1/models` | Models with metadata and effort variants. |
| `POST` | `/v1/chat/completions` | OpenAI chat. |
| `POST` | `/v1/messages` | Anthropic messages. |
| `POST` | `/v1/messages/count_tokens` | Local estimate, no upstream call. |

OpenAI routes read `Authorization: Bearer`, Anthropic routes read `x-api-key`. CORS is
open, so browser tools work.

## 🔧 Configuration

Flags → environment variables → `.env` → defaults. The ones you'll actually touch:

| Variable | Default | |
|:--|:--|:--|
| `PROXY_API_KEY` | `kirogo` | Client key. Warns if left at the default. |
| `SERVER_HOST` · `SERVER_PORT` | `127.0.0.1` · `8000` | Listen address. |
| `KIRO_CREDS_FILE` | auto | Credentials JSON. `~` is expanded. |
| `KIRO_EFFORT_LEVEL` | — | Default reasoning effort. |
| `KIRO_EXPOSE_EFFORT_VARIANTS` | `true` | Advertise `model:level` ids. |
| `LOG_LEVEL` | `INFO` | `DEBUG` logs the exact upstream payload. |

Flags: `-host` `-port` `-dump-models` `-version`. All 22 settings live in
[`.env.example`](.env.example); credential sources and tuning are in
[docs/INTERNALS.md](docs/INTERNALS.md).

## 🧪 Development

```sh
go build ./...       # compile
go test -race ./...  # 520 test functions, 1082 cases
go vet ./... && gofmt -l .
```

Tests make **no network calls** — the backend, credential store and clock are all
substituted, so the suite runs offline and deterministically. Roughly 10k lines of source
against 15k lines of tests, across 9 packages.

> [!NOTE]
> [docs/INTERNALS.md](docs/INTERNALS.md) covers the architecture and the undocumented API
> behaviour kirogo works around — a `systemPrompt` field the backend declares but ignores,
> a catalog on a different host than the docs imply, effort keys that differ per model.
> Worth reading before changing anything on the wire.

Contributions welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). Short version: tests
aren't optional, changes land on both APIs and both streaming modes, identifiers are
English.

## 🔒 Security

kirogo handles live AWS credentials. Please don't open a public issue for a security
problem — see [SECURITY.md](SECURITY.md).

## 📄 License

[MIT](LICENSE) © 2026 [Jasmin](https://github.com/jasminnanda)

An independent, from-scratch implementation. The protocol was worked out by
reverse-engineering the Kiro IDE bundle and verifying behaviour against the live
service.

<sub>Not affiliated with or endorsed by Amazon Web Services. "Kiro", "Amazon Q" and
"CodeWhisperer" are trademarks of Amazon.com, Inc. or its affiliates. Use of kirogo must
comply with your Kiro or AWS terms of service.</sub>
