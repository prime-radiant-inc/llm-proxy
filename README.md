# LLM Proxy

A transparent logging proxy for LLM API traffic. Install once, and every request to Claude, ChatGPT, or other LLM providers is automatically logged for debugging, auditing, and analysis.

## Quick Install

### macOS (Homebrew)

```bash
brew install prime-radiant-inc/tap/llm-proxy
brew services start llm-proxy
```

Restart your shell, and you're done.

### Linux

```bash
curl -fsSL https://raw.githubusercontent.com/prime-radiant-inc/llm-proxy/main/scripts/install.sh | sh
```

Restart your shell, and you're done.

All LLM traffic is now logged to `~/.llm-provider-logs/`.

By default the proxy and explorer bind to loopback only (`localhost`). Log directories are created owner-only (`0700`) and log/database files owner-only (`0600`).

## What It Does

LLM Proxy sits between your LLM clients (Claude Code, Codex, API scripts) and the provider APIs. It:

- **Logs every request and response** to `~/.llm-provider-logs/`
- **Auto-configures your shell** so clients use the proxy automatically
- **Runs as a background service** that starts at login
- **Works with any client** that uses `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL`

## Log Structure

```
~/.llm-provider-logs/
├── api.anthropic.com/
│   └── 2026-01-14/
│       └── 20260114-091523-a1b2c3d4.jsonl
├── api.openai.com/
│   └── 2026-01-14/
│       └── 20260114-102234-i9j0k1l2.jsonl
└── chatgpt.com/
    └── 2026-01-14/
        └── 20260114-111448-m3n4o5p6.jsonl
```

Each session is a JSONL file with request/response pairs, timing information, and metadata.

## Security Defaults

- **Loopback-only listeners by default** for both the proxy and log explorer. Use `--listen-host`, `--explore-listen-host`, or config only when you intentionally need a non-loopback listener.
- **Owner-only local storage**: log directories are created with `0700`; captured log files and `sessions.db` use `0600`.
- **Best-effort secret redaction before persistence/export**: common secret-bearing JSON fields (for example API keys, tokens, passwords, client secrets, authorization-like fields) and common provider token formats are redacted before logs are written to disk or exported to Loki.
- **Important limitation**: this project still captures full request/response bodies for debugging. Redaction reduces common secret leakage, but it cannot reliably identify every sensitive prompt or model output. Treat local logs and any remote export as sensitive data, and define your own retention/deletion policy.

## Remote Push (Loki Export)

Optionally export logs in real-time to [Grafana Loki](https://grafana.com/oss/loki/) for centralized observability. Useful for aggregating logs across ephemeral containers or multiple machines.

### Configuration

Add a `[loki]` section to `~/.config/llm-proxy/config.toml`:

```toml
[loki]
enabled = true
url = "http://loki.example.com:3100/loki/api/v1/push"
auth_token = ""        # Optional: Bearer token for authenticated endpoints
batch_size = 1000      # Entries per batch (default: 1000)
batch_wait = "5s"      # Max time before flushing batch (default: 5s)
retry_max = 5          # Retry attempts on failure (default: 5)
use_gzip = true        # Compress payloads (default: true)
environment = "production"  # Label for filtering in Grafana
```

Or use environment variables:

| Variable | Description |
|----------|-------------|
| `LLM_PROXY_LOKI_ENABLED` | Set to `true` or `1` to enable |
| `LLM_PROXY_LOKI_URL` | Loki push endpoint URL |
| `LLM_PROXY_LOKI_AUTH_TOKEN` | Bearer token for auth |
| `LLM_PROXY_LOKI_BATCH_SIZE` | Entries per batch |
| `LLM_PROXY_LOKI_BATCH_WAIT` | Duration before flush (e.g., `5s`, `10s`) |
| `LLM_PROXY_LOKI_RETRY_MAX` | Max retry attempts |
| `LLM_PROXY_LOKI_USE_GZIP` | Set to `true` or `1` for compression |
| `LLM_PROXY_LOKI_ENVIRONMENT` | Environment label |

### Behavior

- **Non-blocking**: Loki export runs asynchronously and doesn't add latency to proxied requests
- **Graceful degradation**: If Loki is unavailable, local file logging continues unaffected
- **Buffered writes**: Logs are batched and retried on failure; buffer is flushed on shutdown
- **Session correlation**: Logs include session IDs for querying all entries from a single session
- **Transport guardrails**: HTTPS is the default expectation. Plain HTTP requires explicit opt-in and is appropriate only for intentional local/test deployments.

Never place Loki credentials in the URL. Use `auth_token` / `LLM_PROXY_LOKI_AUTH_TOKEN`, private networking, TLS, and least-privilege Grafana/Loki access controls.

## Commands

```bash
llm-proxy --status      # Check if running, show port and log location
llm-proxy --setup       # Full setup (Linux only: installs systemd service)
llm-proxy --setup-shell # Configure shell only (adds eval line to .bashrc/.zshrc)
llm-proxy --uninstall   # Remove service and shell config
```

## How It Works

1. **Service runs in background** on a dynamic port
2. **Port is written** to `~/.local/state/llm-proxy/port`
3. **Shell sources** the eval line: `eval "$(llm-proxy --env)"`
4. **Environment variables** like `ANTHROPIC_BASE_URL` point to the proxy
5. **Clients use the proxy** transparently - no client config needed

The `--env` flag checks if the proxy is running and outputs the appropriate exports. If the proxy isn't running, it outputs nothing, so your shell continues to work normally.

## Supported Providers

- **Anthropic** (Claude, Claude Code)
- **OpenAI** (ChatGPT, Codex, API)
- Any OpenAI-compatible API

The proxy auto-detects ChatGPT OAuth tokens and routes them to the correct backend.

## Manual Usage

If you prefer not to use the background service:

```bash
# Run proxy on a specific port
llm-proxy --port 12071

# Intentional non-loopback access (only when separately protected)
llm-proxy --port 12071 --listen-host 0.0.0.0

# Configure clients manually
export ANTHROPIC_BASE_URL=http://localhost:12071/anthropic/api.anthropic.com
export OPENAI_BASE_URL=http://localhost:12071/openai/api.openai.com
```

## AWS Bedrock Mode

llm-proxy can act as a signing proxy for [AWS Bedrock](https://aws.amazon.com/bedrock/), allowing Claude Code to use Bedrock without managing AWS credentials directly. The proxy receives unsigned Bedrock-format requests, SigV4-signs them, forwards to Bedrock, and decodes the binary eventstream responses for logging while streaming raw bytes back to the client.

### Setup

Set `BEDROCK_REGION` when starting the proxy:

```bash
BEDROCK_REGION=us-west-2 llm-proxy --port 9999
```

The proxy uses the standard AWS SDK credential chain (`~/.aws/credentials`, env vars, instance role, etc.). Your credentials need `bedrock:InvokeModel` and `bedrock:InvokeModelWithResponseStream` permissions.

### Configuring Claude Code

Point Claude Code at the proxy instead of real Bedrock:

```bash
export CLAUDE_CODE_USE_BEDROCK=1
export ANTHROPIC_BEDROCK_BASE_URL=http://localhost:9999
export CLAUDE_CODE_SKIP_BEDROCK_AUTH=1
claude
```

`CLAUDE_CODE_SKIP_BEDROCK_AUTH=1` tells Claude Code to skip its own SigV4 signing since the proxy handles it.

### Health Check

```bash
curl http://localhost:9999/health/bedrock
# {"status":"ok","region":"us-west-2","decode_errors":0}
```

### How It Works

1. Claude Code sends Bedrock-format requests (binary eventstream) to the proxy
2. The proxy extracts the model ID from the URL path, validates it, and SigV4-signs the request
3. The response is streamed back to Claude Code as raw bytes (no transformation)
4. A TeeReader captures the stream for decoding — eventstream frames are parsed, base64-decoded, and fed through the normal logging pipeline (file, Loki, session tracking)

All existing features (session tracking, fingerprinting, Loki export, log explorer) work with Bedrock traffic. Bedrock entries get a `transport=bedrock` label in Loki to distinguish them from direct API traffic.

## Log Explorer

Browse and search your LLM logs with a web UI:

```bash
llm-proxy --explore              # Opens browser to http://localhost:12071
llm-proxy --explore --explore-port 9000  # Use specific port
llm-proxy --explore --explore-listen-host 0.0.0.0  # Intentional non-loopback access
```

Features:
- Session list grouped by date with message counts
- Filter by provider (Anthropic, OpenAI, etc.)
- Conversation view with thinking blocks and tool calls
- Full-text search across all logs
- Raw JSON view for debugging

## Uninstall

```bash
# macOS
brew services stop llm-proxy
brew uninstall llm-proxy
llm-proxy --uninstall

# Linux
llm-proxy --uninstall
rm /usr/local/bin/llm-proxy  # or ~/.local/bin/llm-proxy
```

Logs are preserved in `~/.llm-provider-logs/`. Delete manually if desired.

## Building

```bash
go build -o llm-proxy .
```

## Testing

```bash
# Unit tests
go test -v -short

# Live E2E tests (requires API key in ~/.amplifier/keys.env)
go test -v -run TestLive
```

## License

MIT
