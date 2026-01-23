# Architecture

## Overview

llm-proxy is a transparent HTTP proxy that logs LLM API traffic. It runs as a background service, intercepts requests via environment variable configuration, and writes structured logs for debugging and analysis.

## Layer Boundaries

```
┌─────────────────────────────────────────────────────────────┐
│                         CLI Layer                           │
│  main.go - Flag parsing, mode dispatch, signal handling     │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                       Server Layer                          │
│  server.go - HTTP server, component wiring, health endpoint │
└─────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│  Proxy Layer    │ │  Session Layer  │ │  Logger Layer   │
│  proxy.go       │ │  session.go     │ │  logger.go      │
│  streaming.go   │ │  db.go          │ │                 │
│  urlparse.go    │ │  fingerprint.go │ │                 │
└─────────────────┘ └─────────────────┘ └─────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                      Storage Layer                          │
│  SQLite (sessions.db) + JSONL files (~/.llm-provider-logs/) │
└─────────────────────────────────────────────────────────────┘
```

## Component Responsibilities

### CLI Layer (`main.go`)
- Parse CLI flags and config files
- Dispatch to appropriate mode (service, explore, setup, env)
- Handle graceful shutdown via signal handling
- Write portfile in service mode

### Server Layer (`server.go`)
- Create and wire components: Logger → SessionManager → Proxy
- Expose `/health` endpoint
- Implement `http.Handler` interface
- Manage component lifecycle (Close)

### Proxy Layer (`proxy.go`, `streaming.go`, `urlparse.go`)
- Parse proxy URLs: `/{provider}/{upstream}/{path}`
- Forward requests to upstream APIs
- Handle streaming (SSE) and non-streaming responses
- Detect JWT auth for ChatGPT routing
- Buffer request/response bodies for logging

### Session Layer (`session.go`, `db.go`, `fingerprint.go`)
- Track sessions via client session IDs (from request body)
- Store session metadata in SQLite
- Generate session IDs: `YYYYMMDD-HHMMSS-{random}`
- Manage sequence numbers per session

### Logger Layer (`logger.go`)
- Write JSONL log entries (session_start, request, response, fork)
- Manage file handles per session
- Add metadata: machine ID, timestamps, request IDs
- Obfuscate sensitive headers

### Explorer (`explorer.go`)
- Web UI for browsing logs
- Embedded templates and static assets
- Session list, conversation view, search

## Data Flow

### Request Flow
```
1. Client sends request to proxy URL
2. ParseProxyURL extracts provider, upstream, path
3. SessionManager determines session ID and sequence
4. Logger writes request entry
5. Proxy forwards to upstream
6. Response streamed back through StreamingResponseWriter (if SSE)
7. Logger writes response entry
8. Response returned to client
```

### Session Identification
```
1. Extract client_session_id from request body (e.g., metadata.user_id)
2. If found: look up or create session mapping in SQLite
3. If not found: create new session
4. Return session ID + sequence number for logging
```

## File Organization

```
llm-proxy/
├── main.go           # CLI entry point, flag parsing
├── server.go         # HTTP server wiring
├── proxy.go          # Request forwarding logic
├── streaming.go      # SSE response handling
├── urlparse.go       # Proxy URL parsing
├── session.go        # Session tracking logic
├── db.go             # SQLite operations
├── fingerprint.go    # Message fingerprinting
├── logger.go         # JSONL file logging
├── obfuscate.go      # Header obfuscation
├── config.go         # Config loading (TOML + env)
├── setup.go          # Shell/service installation
├── service.go        # Service management
├── explorer.go       # Web UI
├── parser.go         # Log parsing utilities
├── templates/        # Explorer HTML templates
├── static/           # Explorer static assets
└── docs/
    └── plans/        # Design documents
```

## Dependencies

- **No external HTTP frameworks** - Uses Go stdlib `net/http`
- **SQLite** - `modernc.org/sqlite` (pure Go, no CGO)
- **TOML** - `github.com/pelletier/go-toml/v2`
- **UUID** - `github.com/google/uuid`

## Extension Points

### Adding a New Provider
1. Add to `validProviders` map in `urlparse.go`
2. Add client session ID extraction in `fingerprint.go` if needed
3. Update `extractDeltaText` in `streaming.go` for SSE format

### Adding a New Log Destination
1. Create new exporter implementing log methods (Push, Close, Stats)
2. Create MultiWriter wrapper to fan out to multiple destinations
3. Wire in `server.go` alongside existing Logger
