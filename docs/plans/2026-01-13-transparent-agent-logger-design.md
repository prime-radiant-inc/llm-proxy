# Transparent Agent Logger - Design Document

## Overview

A zero-config Go proxy that transparently logs all traffic between AI clients and their providers (Anthropic, OpenAI-compatible). Supports session tracking with fork/branch detection for debugging, audit, and research purposes.

## Architecture

```
Client (Claude Code, etc.)
    │
    │ ANTHROPIC_BASE_URL=http://localhost:8080/anthropic/api.anthropic.com
    ▼
┌─────────────────────────────┐
│   transparent-agent-logger  │
│                             │
│   - Receives request        │
│   - Logs exact bytes        │
│   - Forwards to upstream    │
│   - Streams response back   │
│   - Logs response chunks    │
│   - Returns to client       │
└─────────────────────────────┘
    │
    ▼
https://api.anthropic.com/v1/messages
```

## URL Format

```
http://localhost:8080/{provider}/{upstream_host}/{original_path}
```

Examples:
- `http://localhost:8080/anthropic/api.anthropic.com/v1/messages`
- `http://localhost:8080/openai/api.openai.com/v1/chat/completions`

The `{provider}` segment determines which session-tracking logic to use (different message formats).

## Configuration

Zero config by default. Optional overrides via TOML file, env vars, or CLI flags.

**Precedence:** CLI flags > Environment variables > Config file > Defaults

**Defaults:**
- Port: `8080`
- Log dir: `./logs`
- DB path: `{log_dir}/sessions.db` (always derived)

**Optional config.toml:**
```toml
port = 8080
log_dir = "./logs"
```

**Environment variables:**
- `AGENT_LOGGER_PORT`
- `AGENT_LOGGER_LOG_DIR`

**CLI flags:**
```bash
./agent-logger --port 8080 --log-dir ./logs
```

## Log File Structure

```
logs/
├── sessions.db                          # SQLite index
├── anthropic/                           # Provider directory
│   ├── 20260113-102345-a7f3.jsonl       # Session file
│   ├── 20260113-102345-a7f3_b1.jsonl    # Branch 1 (fork)
│   └── 20260113-143022-b8e1.jsonl       # Different session
└── openai/
    └── 20260113-110512-c9d2.jsonl
```

**Session file naming:** `{date}-{time}-{random4}.jsonl`
- Branches append `_b{n}`
- When forking: copy parent file up to fork point, rename with branch suffix

## JSONL Entry Format

```json
{"type":"session_start","ts":"...","provider":"anthropic","upstream":"api.anthropic.com"}

{"type":"request","ts":"...","seq":1,"method":"POST","path":"/v1/messages","headers":{...},"body":"<raw bytes>","fingerprint":"abc123"}

{"type":"response","ts":"...","seq":1,"status":200,"headers":{...},"body":"<raw bytes for non-streaming>","chunks":[...],"timing":{"ttfb_ms":234,"total_ms":1892}}

{"type":"fork","ts":"...","from_seq":3,"parent_session":"20260113-102345-a7f3","reason":"message_history_diverged"}
```

**Streaming chunks:**
```json
"chunks": [
  {"ts":"...","delta_ms":0,"raw":"event: message_start\ndata: {...}\n\n"},
  {"ts":"...","delta_ms":45,"raw":"event: content_block_delta\ndata: {...}\n\n"}
]
```

## Comprehensive Metadata Captured

Per request/response:
- Timestamp (ISO 8601)
- Request/response headers
- Exact raw bytes (request body, response body/chunks)
- HTTP status code
- Timing: time to first byte, total time
- Streaming: per-chunk timing (delta from previous)
- Computed: tokens/second (where available from response)
- Request/response size in bytes

## Session Tracking

### SQLite Schema

```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,              -- "20260113-102345-a7f3"
    provider TEXT NOT NULL,           -- "anthropic" | "openai"
    upstream TEXT NOT NULL,           -- "api.anthropic.com"
    created_at TEXT NOT NULL,
    last_activity TEXT NOT NULL,
    last_seq INTEGER NOT NULL,
    last_fingerprint TEXT NOT NULL,
    file_path TEXT NOT NULL
);

CREATE TABLE fingerprints (
    fingerprint TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);

CREATE INDEX idx_fingerprints_session ON fingerprints(session_id);
CREATE INDEX idx_sessions_provider ON sessions(provider);
```

### Session Detection Logic

1. Compute fingerprint of `messages[0:n-1]` (all but last user message)
2. Look up fingerprint in DB

- **No match** → new session: generate ID, create log file, insert into DB
- **Matches latest state** → continuation: append to existing log file, update DB
- **Matches earlier state** → fork: copy parent file to branch, create new session entry

### Fingerprinting

- **For hashing:** Canonicalize JSON (sorted keys, no whitespace) of message history
- **For logging:** Exact raw bytes, no transformation

First turn detection: `len(messages) == 1` → always new session

## API Key Obfuscation

Always enabled, not configurable. In logs, API keys appear as:
```
sk-ant-...XXXX
```
(Prefix + last 4 characters)

Keys pass through to upstream unmodified; obfuscation only in log files.

## Transparency Guarantees

- No modification of request or response content
- No retries or error handling beyond logging
- Errors pass through as-is
- Client sees exactly what upstream returns
- Mid-stream disconnects logged as incomplete with marker

## Endpoints

**Session-tracked:**
- `POST /v1/messages` (Anthropic)
- `POST /v1/chat/completions` (OpenAI)

**Logged but not session-tracked:**
- `POST /v1/messages/count_tokens`
- `POST /v1/messages/batches`
- `GET /v1/messages/batches/*`
- All other paths (pass-through)

## Project Structure

```
transparent-agent-logger/
├── main.go                 # Entry point, CLI parsing
├── config.go               # Config loading (TOML/env/flags)
├── proxy.go                # HTTP reverse proxy, SSE streaming
├── session.go              # Session tracking, fork detection
├── fingerprint.go          # Message canonicalization & hashing
├── logger.go               # JSONL writing, log file management
├── db.go                   # SQLite operations
├── obfuscate.go            # API key redaction
├── providers/
│   ├── provider.go         # Interface for provider-specific logic
│   ├── anthropic.go        # Anthropic message parsing
│   └── openai.go           # OpenAI message parsing
├── config.toml.example
├── go.mod
└── go.sum
```
