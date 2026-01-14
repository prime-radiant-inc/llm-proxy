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
llm-proxy --port 8080

# Configure clients manually
export ANTHROPIC_BASE_URL=http://localhost:8080/anthropic/api.anthropic.com
export OPENAI_BASE_URL=http://localhost:8080/openai/api.openai.com
```

## Log Explorer

Browse and search your LLM logs with a web UI:

```bash
llm-proxy --explore              # Opens browser to http://localhost:8080
llm-proxy --explore --explore-port 9000  # Use specific port
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
