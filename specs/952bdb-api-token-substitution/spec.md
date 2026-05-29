---
runId: 952bdb
feature: api-token-substitution
created: 2026-05-29
status: ready
linear: PRI-1897
---

# Feature: API Token Substitution

## Overview

An opt-in proxy mode where the API token a client presents is a *lookup key*, not the
real upstream credential. When enabled, llm-proxy hands the presented token plus the
identifying context it cheaply has (client host, provider, provider URL) to a configured
**local command**, which returns the real provider key; the proxy caches that mapping in
memory and substitutes the real key onto the outbound request. The real key lives only on
the proxy host — never in the client.

The feature is **off by default** and changes nothing for existing transparent-passthrough
deployments. Its first consumer is Cloud Build (PRI-1888), which must keep the provider
key out of the build box.

## Goals

- Keep the real provider key out of the client; the client holds only an opaque token.
- Pluggable resolution: a configured local command, language-agnostic, so the production
  key server (PRI-1896) can drop in later with no proxy change.
- An in-memory cache keyed by what the resolver actually consumes, with a configurable TTL.
- **Fail-closed**: on any resolution failure, reject the request before it is logged or
  sent upstream; never forward the client's token, never fall back to it.
- Never expose the real key — not in logs, the explorer, or error messages.
- Opt-in: off by default; zero behavior change when disabled.

## Non-Goals

- The key server itself (PRI-1896). v1's resolver command is a static per-(provider,
  endpoint) keyfile reader.
- Minting the tokens/nonces — that is the consumer's job (e.g. the Cloud Build bastion).
- Rate-limiting or quota enforcement (a future resolver / key server may add it).
- Provider-specific request transformation beyond setting the provider's auth header.
- Passing `model` / `client_id` / session id to the resolver in v1 (see Technical
  Decisions — they are not available at the substitution point without new parsing).

## Architecture

**Layer:** Proxy layer (per the constitution). A new component `token_substitution.go`
holds the resolver (which runs the command) and the cache. It is injected into `Proxy`
via the constructor and wired in `server.go`.

**Integration point — early, and on every proxied request.** In `proxy.go` `ServeHTTP`,
substitution runs immediately after `ParseProxyURL` and the request-body buffer
(`proxy.go:283`–`320`), **before** the session/logging block (which is gated behind
`shouldLog`/`isConversationEndpoint`, `proxy.go:341`) and before `client.Do`
(`proxy.go:397`). It must run for **every** proxied request — `/v1/messages`,
`/v1/messages/count_tokens`, `/v1/models`, etc. — because all of them carry the client's
token and a non-conversation endpoint that skipped substitution would forward the opaque
token upstream and 401. Because it runs before the logging block, a fail-closed rejection
never writes a session-start, request log, or turn-start event.

**Resolution context (v1)** — only fields available at that point, JSON on stdin:

```json
{
  "api_token":    "<token the client presented>",
  "client_host":  "<client source address>",
  "provider":     "anthropic",
  "provider_url": "api.anthropic.com"
}
```

`model`, `client_id`, and the llm-proxy session id are **omitted in v1**: they are not
available this early (model is parsed only on the async logging path; the client session
id is computed inside `GetOrCreateSession` and not returned). They become optional context
fields if and when a resolver needs them and the parsing is made available — no pipeline
reordering to chase them now. The token rides on **stdin**, never argv (argv is
world-readable via `ps`).

**Input validation (before resolving).** The proxy rejects the request fail-closed (401)
if `provider` is not in the known-provider allowlist (`validProviders`, `urlparse.go`) or
`provider_url` is not a syntactically valid host (strict charset, no path separators or
shell metacharacters). This protects resolvers that build filesystem paths or shell out
from box-controlled `provider`/`provider_url` values.

**Resolver contract:** the command receives the context JSON on stdin and runs under
`timeout`. Trimmed stdout = the real key on exit 0; non-zero exit, empty output, timeout,
or command-not-found = resolution failure → fail-closed (401).

**Substitution:** on success the proxy **replaces** (HTTP `Set`, not `Add`) the
provider-appropriate auth header on the outbound request with the resolved key, removing
any client-supplied auth header so the opaque token never rides alongside the real key.

**Cache:** an in-memory map keyed by `(provider, provider_url, api_token)` — the fields the
v1 resolver consumes plus the bearer token — value = the resolved key, expiring after
`cache_ttl` and bounded by `cache_size` (oldest evicted on overflow). Thread-safe. If a
future resolver's output depends on more dimensions, the keyed set becomes configurable
(deferred).

## Functional Requirements

- **FR1 — Config, off by default.** An `[api_token_substitution]` section with `enabled`,
  `command`, `cache_ttl`, `cache_size`, `timeout`. Env overrides
  `LLM_PROXY_API_TOKEN_SUBSTITUTION_*`. Precedence env > TOML > defaults (no per-field CLI
  flags — consistent with the existing Loki section).
- **FR2 — Disabled is transparent.** When `enabled = false`, the request path is
  byte-for-byte identical to today's passthrough.
- **FR3 — Resolve early, every request.** When enabled, for every proxied request the proxy
  validates `provider`/`provider_url`, builds the v1 context, checks the cache, and on a
  miss runs `command` with the context JSON on stdin under `timeout` — all before the
  session/logging block and before `client.Do`.
- **FR4 — Substitute.** On success the proxy `Set`s the provider-appropriate auth header
  (`x-api-key` for Anthropic key-style auth, `Authorization: Bearer` where the client used
  it) on the outbound request to the resolved key, replacing/removing the client's auth
  header.
- **FR5 — Fail closed, before logging.** On invalid input, non-zero exit, empty stdout,
  timeout, or command-not-found, the proxy returns `401`, does **not** contact the upstream,
  and does **not** emit any session/request/turn log for that request. Error responses and
  log lines never contain the token or any key.
- **FR6 — Never expose the real key.** The resolved key appears in no log, JSONL entry, the
  explorer, or any error message. (The request logger records inbound — obfuscated —
  headers, not the mutated outbound header.) A test asserts the resolved key is absent from
  request- and response-header logs and from all error strings.
- **FR7 — Cache.** Resolved keys are cached by `(provider, provider_url, api_token)`, reused
  within `cache_ttl`, evicted past it or when `cache_size` is exceeded; concurrent requests
  are safe.

## Technical Decisions

- **A command, not a built-in keystore.** Keeps llm-proxy general and language-agnostic and
  gives a clean seam for the future key server (PRI-1896). Matches the "run a local command
  to swap the token" model.
- **JSON on stdin.** Carries the context and keeps the token off argv; extensible — new
  fields need no contract change.
- **v1 context is only what's available early.** Substitution must run before the session
  block (so it covers non-conversation endpoints and precedes logging), and `model` /
  `client_id` / session id are not available there. Including them would require new parsing
  the v1 resolver doesn't use, so they are deferred.
- **Cache key = `(provider, provider_url, api_token)`.** Matches what the v1 resolver
  consumes (provider + endpoint) plus the bearer token, so it is correct and stable per
  session (one token per box) rather than thrashing on per-request fields like `model`.
- **Fail-closed is a deliberate primary-path gate.** When enabled, substitution is on the
  critical path: a resolution failure rejects *that* request (401) and never forwards the
  opaque token. This is intentionally *not* graceful degradation. The only "don't break the
  primary path" guarantee is operational — one request's failure never crashes the proxy or
  affects other requests.
- **Validate box-controlled inputs.** `provider`/`provider_url` come from the client URL, so
  they are validated against an allowlist/charset before any resolver invocation.
- **Off by default.** Existing passthrough deployments are unaffected.

## Files to Create / Modify

| File | Change |
|------|--------|
| `token_substitution.go` (new) | Resolver (exec the command with the context JSON on stdin, under `timeout`) + the in-memory cache keyed by `(provider, provider_url, api_token)` with TTL/size bounds; thread-safe. |
| `config.go` | Add the `APITokenSubstitution` config struct, defaults, and `LLM_PROXY_API_TOKEN_SUBSTITUTION_*` env loading (env > TOML > defaults). |
| `config.toml.example` | Document the `[api_token_substitution]` section. |
| `proxy.go` | Run validation + substitution early (after `ParseProxyURL`/body buffer, before the `shouldLog` block and `client.Do`); `Set` the outbound auth header; fail-closed before logging. Add the resolver to the `Proxy` struct. |
| `server.go` | Construct the resolver from config and inject it into `Proxy`. |
| `token_substitution_test.go` (new) | Table-driven unit tests (mocked resolver) + an integration test with a real script: success, invalid provider/provider_url, non-zero exit, empty output, timeout, command-not-found, cache hit / expiry / eviction, concurrency, and a non-conversation endpoint (`count_tokens`) getting substituted. |

## Success Criteria

- With the feature enabled and a resolver script, every proxied request — including
  `count_tokens` and `models` — reaches the upstream carrying the real key, and responses
  are normal.
- A resolver that exits non-zero / empty / times out, or an invalid `provider`/`provider_url`
  → the request gets `401`, the upstream is never contacted, and no session/request/turn log
  is written for it.
- The real key never appears in any log, JSONL entry, the explorer, or an error message.
- Within `cache_ttl`, repeated requests for the same `(provider, provider_url, token)` do not
  re-run the command.
- With the feature disabled (default), behavior is identical to today.
- New code meets the constitution's testing bar (table-driven, ≥80% coverage,
  concurrency-safe).

## Constitution Reference

Follows `@docs/constitutions/current/` — proxy-layer placement; secrets-never-logged; the
config precedence and `LLM_PROXY_*` env convention; and the testing standards (table-driven
unit tests, live/integration separation, coverage, concurrency). Note: unlike the Loki
exporter (which fails *open* as a degradable side-feature), token substitution when enabled
is a primary-path gate and fails *closed* by design; the constitution's "don't break the
primary path" rule is honored only in that a single request's failure never crashes the
proxy or affects other requests.
