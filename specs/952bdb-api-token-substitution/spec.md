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
identifying context it already has (client host, provider, provider URL, model, client
id) to a configured **local command**, which returns the real provider key; the proxy
caches that mapping in memory with a TTL and substitutes the real key onto the outbound
request. The real key lives only on the proxy host — never in the client.

The feature is **off by default** and changes nothing for existing transparent-passthrough
deployments. Its first consumer is Cloud Build (PRI-1888), which must keep the provider
key out of the build box.

## Goals

- Keep the real provider key out of the client; the client holds only an opaque token.
- Pluggable resolution: a configured local command, language-agnostic, so the production
  key server (PRI-1896) can drop in later with no proxy change.
- Per-identity in-memory cache with a configurable TTL, to spare the resolver.
- **Fail-closed**: on any resolution failure, reject the request; never forward the
  client's token, never fall back to it.
- Never log the real key.
- Opt-in: off by default; zero behavior change when disabled.

## Non-Goals

- The key server itself (PRI-1896). v1's resolver command is a static per-(provider,
  endpoint) keyfile reader.
- Minting the tokens/nonces — that is the consumer's job (e.g. the Cloud Build bastion).
- Rate-limiting or quota enforcement (a future resolver / key server may add it).
- Provider-specific request transformation beyond setting the provider's auth header.

## Architecture

**Layer:** Proxy layer (per the constitution). A new component `token_substitution.go`
holds the resolver (which runs the command) and the cache. It is injected into `Proxy`
via the constructor and wired in `server.go`.

**Integration point:** in `proxy.go` `ServeHTTP`, the resolver runs **after the request
context is available** — provider + upstream from `ParseProxyURL`, and model + client
session id from the body parse around `GetOrCreateSession` — and **before the upstream
request is sent** (`p.client.Do`). It overwrites the outbound request's provider auth
header with the resolved key. (This is deliberately later than the initial `copyHeaders`
at `proxy.go:330`, because model/client-id are not parsed until session handling; the
substitution replaces the copied auth header on the already-built `proxyReq` just before
the send.)

**Resolution context** (JSON on stdin to the command):

```json
{
  "api_token":    "<token the client presented>",
  "client_host":  "<client source address>",
  "provider":     "anthropic",
  "provider_url": "api.anthropic.com",
  "model":        "claude-…",
  "client_id":    "<client_session_id, when present>",
  "session":      "<llm-proxy session id>"
}
```

Fields not yet available at resolution time are omitted (no extra parsing to chase them).
The token rides on **stdin**, never argv (argv is world-readable via `ps`).

**Resolver contract:** trimmed stdout = the real key on exit 0; non-zero exit, empty
output, or timeout = resolution failure.

**Cache:** an in-memory map keyed by a canonical hash of the resolution-context object
(so a cached key can never be returned for a different identity), value = the resolved
key, expiring after `cache_ttl` and bounded by `cache_size` (oldest evicted on overflow).
Thread-safe.

## Functional Requirements

- **FR1 — Config, off by default.** An `[api_token_substitution]` section with `enabled`,
  `command`, `cache_ttl`, `cache_size`, `timeout`. Env overrides
  `LLM_PROXY_API_TOKEN_SUBSTITUTION_*`. Precedence CLI > env > TOML > defaults.
- **FR2 — Disabled is transparent.** When `enabled = false`, the request path is
  byte-for-byte identical to today's passthrough.
- **FR3 — Resolve.** When enabled, the proxy builds the resolution context, checks the
  cache, and on a miss runs `command` with the context JSON on stdin under `timeout`.
- **FR4 — Substitute.** On success, the proxy sets the provider-appropriate auth header
  (`x-api-key` for Anthropic key-style auth, `Authorization: Bearer` where the client used
  it) on the outbound request to the resolved key, replacing whatever the client sent.
- **FR5 — Fail closed.** On non-zero exit, empty stdout, timeout, or command-not-found,
  the proxy returns `401` and does **not** contact the upstream.
- **FR6 — Never log the real key.** The resolved key is written to no log, JSONL entry, or
  the explorer; the inbound token stays obfuscated as today.
- **FR7 — Cache.** Resolved keys are cached by the context hash, reused within `cache_ttl`,
  evicted past it or when `cache_size` is exceeded; concurrent requests are safe.

## Technical Decisions

- **A command, not a built-in keystore.** Keeps llm-proxy general and language-agnostic and
  gives a clean seam for the future key server (PRI-1896). Matches the "run a local command
  to swap the token" model.
- **JSON on stdin.** Carries the open-ended context and keeps the token off argv;
  extensible — new fields need no contract change.
- **Cache key = hash of the full resolution context.** Correct-by-construction: the cache
  can never hand back a key resolved for a different identity. The fields are session-stable
  in practice so hit rate stays high; making the keyed dimensions configurable is deferred.
- **Fail-closed.** A resolution failure rejects only that request; the client's opaque token
  is never forwarded upstream (it is not a real key) and there is no fallback. Per the
  constitution, this secondary feature never breaks the primary path — the proxy keeps
  serving other requests.
- **Off by default.** Existing passthrough deployments are unaffected.

## Files to Create / Modify

| File | Change |
|------|--------|
| `token_substitution.go` (new) | Resolver (exec the command with the context JSON on stdin, under `timeout`) + the in-memory TTL/size cache; thread-safe. |
| `config.go` | Add the `APITokenSubstitution` config struct, defaults, and `LLM_PROXY_API_TOKEN_SUBSTITUTION_*` env loading. |
| `config.toml.example` | Document the `[api_token_substitution]` section. |
| `proxy.go` | Build the resolution context after session/model parse; call the resolver; set the outbound auth header before `client.Do`; fail-closed. Add the resolver to the `Proxy` struct. |
| `server.go` | Construct the resolver from config and inject it into `Proxy`. |
| `token_substitution_test.go` (new) | Table-driven unit tests (mocked resolver) + an integration test with a real script: success, non-zero exit, empty output, timeout, command-not-found, cache hit / expiry / eviction, concurrency. |

## Success Criteria

- With the feature enabled and a resolver script, a request presenting an arbitrary token
  reaches the upstream carrying the real key and the response is normal.
- A resolver that exits non-zero / empty / times out → the request gets `401` and the
  upstream is never contacted.
- The real key never appears in any log, JSONL entry, or the explorer.
- Within `cache_ttl`, repeated requests for the same identity do not re-run the command.
- With the feature disabled (default), behavior is identical to today.
- New code meets the constitution's testing bar (table-driven, ≥80% coverage,
  concurrency-safe).

## Constitution Reference

Follows `@docs/constitutions/current/` — proxy-layer placement; the "secondary features
must not break the primary path" and fail-closed rules; secrets-never-logged; the config
precedence and `LLM_PROXY_*` env convention; and the testing standards (table-driven unit
tests, live/integration separation, coverage, concurrency).
