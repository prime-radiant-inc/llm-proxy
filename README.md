# Transparent Agent Logger

A zero-config Go proxy that transparently logs all traffic between AI clients and their providers.

## Quick Start

```bash
# Run with defaults (port 8080, logs in ./logs)
./agent-logger

# Configure your client
export ANTHROPIC_BASE_URL=http://localhost:8080/anthropic/api.anthropic.com

# Use Claude as normal - all traffic is logged
claude "Hello world"
```

## URL Format

```
http://localhost:8080/{provider}/{upstream}/{path}
```

Supported providers:
- `anthropic` - Anthropic API (Claude)
- `openai` - OpenAI-compatible APIs

## Configuration

### CLI Flags

```bash
./agent-logger --port 9000 --log-dir /var/log/agent-logger
```

### Environment Variables

```bash
AGENT_LOGGER_PORT=9000
AGENT_LOGGER_LOG_DIR=/var/log/agent-logger
```

### Config File

```toml
# config.toml
port = 9000
log_dir = "/var/log/agent-logger"
```

Precedence: CLI flags > Environment variables > Config file > Defaults

## Log Format

Logs are stored as JSONL files in `{log_dir}/{provider}/{session_id}.jsonl`.

Each file contains:
- `session_start` - Session metadata
- `request` - Full request with obfuscated API keys
- `response` - Full response with timing data

Session tracking detects conversation continuations and forks.

## Building

```bash
go build -o agent-logger .
```

## Testing

```bash
# Unit tests
go test -v -short

# Live E2E tests (requires API key in ~/.amplifier/keys.env)
go test -v -run TestLive
```
