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

This spec also includes one small **related proxy change** the shared-host deployment needs:
a configurable listen address (see "Related change" below).

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
- Passing `model` / `client_id` / session id to the resolver in v1 (not available at the
  substitution point without new parsing).

## Architecture

**Layer:** Proxy layer (per the constitution). A new component `token_substitution.go`
holds the resolver (which runs the command) and the cache. It is injected into `Proxy`
via the constructor and wired in `server.go`.

**Where the token is read.** The presented token is taken from the provider's auth header —
`x-api-key` for Anthropic key-style auth, `Authorization: Bearer <token>` where the client
uses it — reusing the existing key-header logic (`obfuscate.go` `isAPIKeyHeader`). If both
headers are present, `x-api-key` takes precedence. The header the token was read from is the
one replaced in FR4, and any other client-supplied auth header is stripped so the opaque
token never survives on the outbound request.

**Integration point — resolve early, on every request; replace the header after copy.** In
`proxy.go` `ServeHTTP`:
- Read the token and (after `ParseProxyURL`, `proxy.go:283`, and after any provider-specific
  upstream remap — see below) build the context; validate; resolve. This is independent of
  the session/logging block (gated by `shouldLog`/`isConversationEndpoint`, `proxy.go:341`),
  so it runs for every **`ParseProxyURL`-routed** endpoint — `/v1/messages`, `count_tokens`,
  `models`, etc. (a non-conversation endpoint that skipped substitution would forward the
  opaque token and 401). Bedrock `/model/*` requests return at `proxy.go:276` *before*
  `ParseProxyURL` and are out of substitution scope in v1; Cloud Build is Anthropic-only.
- **Apply the substitution by `Set`ting the auth header on the outbound `proxyReq` *after*
  `copyHeaders` (`proxy.go:330`)** — `copyHeaders` copies the client's headers onto `proxyReq`,
  so the replacement must follow it or it is clobbered — and before the `shouldLog` block
  (341) and `client.Do` (397). Because resolution and the fail-closed gate precede the
  logging block, a rejected request writes no session-start, request, or turn-start log.

**`provider_url` value.** `provider_url` is the `upstream` host **as used for the outbound
request**, i.e. captured after any provider-specific remap (for OpenAI JWT auth the proxy
rewrites `upstream` `api.openai.com → chatgpt.com` at `proxy.go:292-297`). For the v1
Anthropic consumer there is no remap, so `provider_url` is `api.anthropic.com`.

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
available this early (model is parsed only on the async logging path; the client session id
is computed inside `GetOrCreateSession` and not returned). They become optional context
fields if and when a resolver needs them and the parsing is made available — no pipeline
reordering now. The token rides on **stdin**, never argv.

**Input validation (before resolving).** `provider` is already validated by `ParseProxyURL`
(unknown providers are rejected `400` at `proxy.go:285`). The feature additionally validates
`provider_url` against a strict host charset (no path separators or shell metacharacters)
and rejects fail-closed if it fails — protecting resolvers that build filesystem paths or
shell out from box-controlled values.

**Resolver contract:** the command receives the context JSON on stdin and runs under
`timeout`. **Trimmed stdout is the real key itself (not a path or filename)** on exit 0. A
resolution failure fails closed, with the status reflecting the cause: an input-validation or
allowlist/auth rejection (clean non-zero exit, no key) → `401` (non-retryable); a transient
resolver fault (timeout, spawn failure, command-not-found, or empty stdout) → `502`
(retryable), so a client backs off instead of treating it as a bad credential.

**Cache:** an in-memory map keyed by `(provider, provider_url, api_token)` — the fields the
v1 resolver consumes plus the bearer token — value = the resolved key, expiring after
`cache_ttl` and bounded by `cache_size` (oldest evicted on overflow). Thread-safe. If a
future resolver's output depends on more dimensions, the keyed set becomes configurable
(deferred). Any context field passed to the resolver but absent from the cache key (e.g.
`client_host` in v1) must not influence the resolved key until the keyed set is configurable.

## Related change: configurable listen address

The proxy currently binds `localhost` only (`main.go:241`, deliberately, because it has no
auth). The shared-host Cloud Build deployment needs it to bind the host's bridge gateway IP.
Add a proxy-wide `listen_host` config (env `LLM_PROXY_LISTEN_HOST`), **default `localhost`**
(unchanged behavior). This is general proxy config, not substitution-specific. Binding a
non-loopback address re-introduces the unauthenticated-access concern the localhost default
guards against, so it is intended to be used only behind a host firewall / isolated bridge
(the Cloud Build spec specifies the INPUT rule); document that in the config comment.

## Functional Requirements

- **FR1 — Config, off by default.** An `[api_token_substitution]` section with `enabled`,
  `command`, `cache_ttl`, `cache_size`, `timeout`; plus the top-level `listen_host`. Env
  overrides `LLM_PROXY_API_TOKEN_SUBSTITUTION_*` and `LLM_PROXY_LISTEN_HOST`. Precedence
  env > TOML > defaults (no per-field CLI flags — consistent with the Loki section).
- **FR2 — Disabled is transparent.** When `enabled = false`, the request path is
  byte-for-byte identical to today's passthrough.
- **FR3 — Resolve early, every request.** When enabled, for every proxied request the proxy
  reads the token, validates `provider_url`, builds the v1 context, checks the cache, and on
  a miss runs `command` with the context JSON on stdin under `timeout` — independent of the
  session/logging block and before `client.Do`.
- **FR4 — Substitute.** On success the proxy `Set`s the provider auth header (the one the
  token came from) on `proxyReq` to the resolved key, **after `copyHeaders` (proxy.go:330)**,
  removing the client's value so the opaque token never rides alongside the real key.
- **FR5 — Fail closed, before logging.** On a resolution failure the proxy returns `401`
  (input-validation/allowlist rejection) or `502` (transient resolver fault: timeout, spawn
  failure, command-not-found, empty stdout), does **not** contact the upstream, and writes
  **no** session/request/turn log for that request. Error responses and log lines never
  contain the token or any key.
- **FR6 — Never expose the real key.** The resolved key appears in no log, JSONL entry, the
  explorer, or any error message. (The request logger records inbound — obfuscated — headers
  `r.Header`, not the mutated `proxyReq.Header`.) A test asserts the resolved key is absent
  from request- and response-header logs and from all error strings.
- **FR7 — Cache.** Resolved keys are cached by `(provider, provider_url, api_token)`, reused
  within `cache_ttl`, evicted past it or when `cache_size` is exceeded; concurrent-safe.

## Technical Decisions

- **A command, not a built-in keystore.** Keeps llm-proxy general and gives a clean seam for
  the future key server (PRI-1896). Matches the "run a local command to swap the token" model.
- **Resolver emits the key, not a reference.** stdout is used verbatim as the credential, so
  the resolver must print the key's bytes (a file-backed resolver `cat`s its keyfile).
- **JSON on stdin.** Carries the context, keeps the token off argv, and is extensible.
- **v1 context is only what's available early.** Substitution must precede the session block
  (to cover non-conversation endpoints and precede logging); `model`/`client_id`/session id
  aren't available there and the v1 resolver doesn't use them, so they're deferred.
- **Cache key = `(provider, provider_url, api_token)`.** Matches what the v1 resolver consumes
  plus the bearer token — correct and stable per bearer token, not thrashing on per-request
  fields like `model`.
- **Fail-closed is a deliberate primary-path gate.** When enabled, a resolution failure
  rejects *that* request (401) and never forwards the opaque token. This is intentionally not
  graceful degradation; the only "don't break the primary path" guarantee is that one
  request's failure never crashes the proxy or affects other requests.
- **Validate box-controlled inputs.** `provider_url` comes from the client URL, so it is
  charset-validated before any resolver invocation (provider is already gated by `ParseProxyURL`).
- **Off by default.** Existing passthrough deployments are unaffected.

## Files to Create / Modify

| File | Change |
|------|--------|
| `token_substitution.go` (new) | Resolver (exec the command with the context JSON on stdin, under `timeout`) + the in-memory cache keyed by `(provider, provider_url, api_token)` with TTL/size bounds; thread-safe. |
| `config.go` | Add the `APITokenSubstitution` config struct + defaults + `LLM_PROXY_API_TOKEN_SUBSTITUTION_*` env loading; add top-level `ListenHost` (`LLM_PROXY_LISTEN_HOST`, default `localhost`). |
| `main.go` | Use the configured `listen_host` for the listener (currently hard-coded `localhost:%d` at line 241). |
| `config.toml.example` | Document `[api_token_substitution]` and `listen_host`. |
| `proxy.go` | Read the token; validate `provider_url`; resolve (after `ParseProxyURL`/remap); `Set` the auth header on `proxyReq` after `copyHeaders` (330), before the `shouldLog` block and `client.Do`; fail-closed before logging. Add the resolver to the `Proxy` struct. |
| `server.go` | Construct the resolver from config and inject it into `Proxy`. |
| `token_substitution_test.go` (new) | Table-driven unit tests (mocked resolver) + an integration test with a real script: success, invalid `provider_url`, non-zero exit, empty output, timeout, command-not-found, cache hit/expiry/eviction, concurrency, and a `count_tokens` (non-conversation) request being substituted. |

## Success Criteria

- With the feature enabled and a resolver script that prints a key, every proxied request —
  including `count_tokens`/`models` — reaches the upstream carrying the real key.
- A resolver that exits non-zero / empty / times out, or an invalid `provider_url`, → `401`,
  no upstream contact, and no session/request/turn log for that request.
- The real key never appears in any log, JSONL entry, the explorer, or an error message.
- Within `cache_ttl`, repeated requests for the same `(provider, provider_url, token)` don't
  re-run the command.
- With the feature disabled (default), and with `listen_host` unset (default `localhost`),
  behavior is identical to today.
- New code meets the constitution's testing bar (table-driven, ≥80% coverage, concurrency-safe).

## Constitution Reference

Follows `@docs/constitutions/current/` — proxy-layer placement; secrets-never-logged; config
precedence + `LLM_PROXY_*` env convention; testing standards (table-driven, live/integration
separation, coverage, concurrency). Unlike the Loki exporter (which fails *open* as a
degradable side-feature), token substitution when enabled is a primary-path gate and fails
*closed* by design; the constitution's "don't break the primary path" rule is honored only in
that a single request's failure never crashes the proxy or affects other requests.
