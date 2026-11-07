# gridtie

**Local-first LLM routing.** gridtie sits in front of your model providers and answers every request from
your own hardware when it can, and from the cloud only when it can't.

Like a grid-tied solar home: you run on your own power, and draw from the grid only when the panels can't keep up.

```
                    ┌──────────────┐  ready? ──▶  local (llama-swap, …)
 your apps ──────▶  │   gridtie    │
 (OpenAI API)       └──────────────┘  not yet ─▶  cloud (OpenRouter, …)
                                       └─ and wakes the local one
```

## Why

Running models at home is great until the GPU isn't ready: the machine is asleep to save power, the server
scaled to zero, or the model you picked is still loading. Every request then waits. gridtie checks, **per
request**, whether your local model can answer right now:

- **Ready:** the request goes local, as if gridtie weren't there.
- **Not ready:** the request goes to the next provider you configured, usually a cloud equivalent, so it is
  answered at once. gridtie also nudges the local side awake.
- **The next request after local is ready** goes local again.

Chat APIs are stateless: clients send the whole conversation with every message. That makes the hand-off
seamless. The local model picks up the conversation the cloud started, with full context. A reply that has
started streaming always finishes where it started.

## Features

- **OpenAI-compatible:** `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, with streaming.
  Point any OpenAI client at it.
- **Readiness-aware routing:** knows whether the requested model is loaded, not just whether a server
  answers. Supports [llama-swap](https://github.com/mostlygeek/llama-swap) today.
- **Wake hooks:** calls a URL to start the local side (a scale-to-zero proxy, a Wake-on-LAN service, …), and
  asks llama-swap to load the requested model.
- **Stays local when you want it to:** clients or single requests can opt out of the cloud and wait for local.
- **Survives the cloud saying no:** out of credits, bad key, rate limited or down? The request falls back to
  waiting for local, and gridtie skips that provider for a while.
- **Observable:** every response says which provider answered, in the `X-Gridtie-Provider` header and in the
  response's `usage` (`provider`, `upstream_model`). Clients that show usage, like open-webui's ⓘ icon under each
  reply, show it too. There are also Prometheus metrics and a `/status` page.

## Quick start

```yaml
# config.yaml
wakeOnModels: true                 # opening a chat UI starts the wake-up
providers:
  local:
    baseURL: http://llama-swap:8080/v1
    readiness:
      llamaSwap: http://llama-swap:8080
    wake:
      url: http://my-waker:9090/wake  # optional
  openrouter:
    baseURL: https://openrouter.ai/api/v1
    apiKeyEnv: OPENROUTER_API_KEY
models:
  qwen3-coder:
    name: Qwen3 Coder
    aliases: [coder]
    routes:                          # in order of preference
      - {provider: local, model: qwen3-coder}
      - {provider: openrouter, model: qwen/qwen3-coder}
```

```sh
docker run -p 8080:8080 -p 9090:9090 -e OPENROUTER_API_KEY \
  -v $PWD/config.yaml:/etc/gridtie/config.yaml ghcr.io/tpural/gridtie:latest
```

Point your client at `http://localhost:8080/v1`. A Kubernetes example is in [docs/kubernetes.yaml](docs/kubernetes.yaml).

## How a request is routed

1. The `model` is looked up by ID or alias. Unknown models get a 404.
2. Routes are checked in order. The first one whose provider is **ready for that model** and isn't cooling
   down wins. Providers without a `readiness` check are always ready.
3. If the preferred (first) route was skipped, gridtie wakes it in the background, at most every 30 seconds:
   - if the provider is down, it calls the `wake` hook;
   - if it is up with a different model loaded, it asks llama-swap to load the requested one.
4. If a provider rejects the request (`401`, `402`, `403`, `429` or `5xx`) or can't be reached, the next route
   is tried. Nothing has been sent to the client yet, so this is invisible.
5. The preferred route is always the last resort: when nothing else can answer, the request goes there and waits.

Only the `model` field of the request body is changed. Messages, tools, images and every other field
are forwarded untouched.

## Keeping things local

Some clients should never leave home, for example agents working on private data. There are three ways:

- **Per request:** send `X-Gridtie-Route: local-only` (or `wait`).
- **Per client:** add rules. The first matching rule applies:
  ```yaml
  rules:
    - {header: "User-Agent=^n8n", localOnly: true}
    - {cidr: 10.20.0.0/16, localOnly: true}
    - {host: private-llm.example.com, localOnly: true}
  ```
- **Per model:** give it only a local route.

Or point those clients straight at your local server. gridtie is opt-in.

## When the cloud can't answer

| Response | What gridtie does |
|---|---|
| `402` (out of credits), `401`/`403` (bad key) | Tries the next route, and skips the provider for `cooldown` (default 10m) |
| `429` (rate limited) | Tries the next route, and skips the provider until `Retry-After` (default 1m) |
| `5xx` or unreachable | Tries the next route |

Because the preferred route is always the last resort, a cloud outage means "wait for local", never an error,
as long as local eventually comes up.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:8080` | API address |
| `adminListen` | `:9090` | `/healthz`, `/metrics`, `/status` |
| `cooldown` | `10m` | How long to skip a provider after `401`/`402`/`403` |
| `wakeOnModels` | `false` | Wake preferred providers that are down when a client lists models. Chat UIs list models when they open, so the warm-up starts before you type. Never loads models. |
| `providers.<name>.baseURL` | | OpenAI-compatible base URL, including `/v1` |
| `providers.<name>.apiKeyEnv` | | Environment variable holding the API key, sent as a Bearer token. Without one, the client's `Authorization` header is forwarded. |
| `providers.<name>.headers` | | Extra request headers, e.g. OpenRouter's `HTTP-Referer` |
| `providers.<name>.readiness.llamaSwap` | | llama-swap base URL, used for `GET /running`. Use an address that doesn't wake anything. |
| `providers.<name>.wake.url` / `.method` | `POST` | Called when the provider is down and preferred |
| `models.<id>.name` / `.aliases` | | Display name and other IDs clients may use |
| `models.<id>.routes[]` | | `{provider, model}` in order of preference, where `model` is the provider's own model ID |
| `rules[]` | | One of `host`, `cidr`, `header` (`Name=regexp`), plus `localOnly` |

**Metrics:**
- `gridtie_requests_total{model,provider,code}`;
- `gridtie_skips_total{provider,reason}`;
- `gridtie_request_seconds{provider}`.

## Example: a GPU that sleeps when idle

A home server with a big GPU uses a lot of power doing nothing, so it suspends when idle. A scale-to-zero
proxy in front of llama-swap wakes it on the first request, and that takes about a minute. With gridtie in
front:

1. You open your chat UI. It lists models, and gridtie calls the proxy's wake endpoint (`wakeOnModels`).
2. You send a message while the GPU is still waking. llama-swap isn't reachable, so the message goes to the
   same model family on OpenRouter and is answered in a second or two.
3. Around a minute later the server is up and llama-swap has loaded the model.
4. Your next message goes local, and the local model continues the conversation with the full context.
5. Background agents carry `localOnly` rules, so they wait for the GPU instead of sending private data to the cloud.

## Development

```sh
make test   # go test -race ./...
make lint   # golangci-lint
make build
```

PR titles are conventional commits. Merging to `main` releases automatically. Comment `/rc` on a PR to
build a release-candidate image.

## License

Apache 2.0
