# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Constitution

All code must follow the architectural rules and patterns defined in:
**@docs/constitutions/current/**

See [architecture.md](docs/constitutions/current/architecture.md) for layer boundaries,
[patterns.md](docs/constitutions/current/patterns.md) for common patterns, and
[testing.md](docs/constitutions/current/testing.md) for testing standards.

## Project Overview

llm-proxy is a transparent logging proxy for LLM API traffic. It intercepts requests to Claude, ChatGPT, and other LLM providers, logging full request/response bodies for debugging, auditing, and analysis. Designed to run as a background service that auto-configures shell environment variables.

## Build and Test Commands

```bash
# Build
go build -o llm-proxy .

# Run all tests
go test ./...

# Run unit tests only (skip live API tests)
go test -v -short ./...

# Run a single test
go test -v -run TestFunctionName ./...

# Run live E2E tests (requires API key in ~/.amplifier/keys.env)
go test -v -run TestLive ./...

# Run proxy manually
./llm-proxy --port 8080

# Run as background service (dynamic port, writes portfile)
./llm-proxy --service

# Start log explorer web UI
./llm-proxy --explore
```

## Architecture

### Request Flow

```
Client → Proxy (ServeHTTP) → ParseProxyURL → Upstream API
              │
              ├─ SessionManager (session tracking via SQLite)
              ├─ Logger (JSONL file per session)
              └─ StreamingResponseWriter (for SSE responses)
```

### URL Format

Proxy URLs follow the pattern: `/{provider}/{upstream}/{path}`
- Example: `/anthropic/api.anthropic.com/v1/messages`
- Providers: `anthropic`, `openai`
- ChatGPT OAuth tokens are auto-detected and routed to `chatgpt.com/backend-api/codex`

### Key Components

| File | Purpose |
|------|---------|
| `main.go` | CLI flags, service modes (--service, --env, --explore, --setup) |
| `server.go` | HTTP server wiring: Logger → SessionManager → Proxy |
| `proxy.go` | Request proxying, header copying, response handling |
| `session.go` | Session tracking via client session IDs from request bodies |
| `logger.go` | JSONL logging with machine ID, timing, request/response bodies |
| `db.go` | SQLite session database (sessions.db) |
| `streaming.go` | SSE response handling, chunk accumulation |
| `fingerprint.go` | Message fingerprinting for session correlation |
| `explorer.go` | Web UI for browsing logs (embedded templates/static) |
| `urlparse.go` | Proxy URL parsing and validation |
| `config.go` | TOML config + environment variable loading |
| `setup.go` | Shell integration, systemd service installation |

### Session Tracking

Sessions are tracked via `client_session_id` extracted from request bodies (e.g., Claude Code's `metadata.user_id`). Each session gets:
- Unique ID: `YYYYMMDD-HHMMSS-{random}`
- SQLite record tracking sequence numbers
- JSONL file at `~/.llm-provider-logs/{upstream}/{date}/{session}.jsonl`

### Log Entry Types

- `session_start` - New session initiated
- `request` - Full request body, headers (obfuscated), timing
- `response` - Full response body or streaming chunks, status, timing
- `fork` - Session forked due to message history divergence

## Environment Variables

| Variable | Description |
|----------|-------------|
| `LLM_PROXY_PORT` | Port to listen on |
| `LLM_PROXY_LOG_DIR` | Log directory (default: `~/.llm-provider-logs/`) |
| `ANTHROPIC_BASE_URL` | Set by `--env` to route Anthropic traffic through proxy |
| `OPENAI_BASE_URL` | Set by `--env` to route OpenAI traffic through proxy |

## File Locations

- Logs: `~/.llm-provider-logs/{upstream}/{YYYY-MM-DD}/{session}.jsonl`
- Session DB: `~/.llm-provider-logs/sessions.db`
- Portfile: `~/.local/state/llm-proxy/port`
- Config: `~/.config/llm-proxy/config.toml` (optional)
