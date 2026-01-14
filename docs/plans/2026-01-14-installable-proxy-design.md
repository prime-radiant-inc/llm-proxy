# LLM Proxy: Installable Auto-Configuring Proxy

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the proxy installable via `brew install` (macOS) or `curl | sh` (Linux), with automatic shell configuration so LLM traffic is logged transparently.

**Architecture:** System service runs the proxy on a dynamic port, writes port to state file, shell sources env script that reads port and sets `*_BASE_URL` vars.

**Tech Stack:** Go binary, launchd (macOS), systemd --user (Linux), shell script integration

---

## Installation Flow

### macOS (Homebrew)

```
brew install llm-proxy
brew services start llm-proxy
# Restart shell
```

### Linux (curl script)

```
curl -fsSL https://llm-proxy.dev/install.sh | sh
# Restart shell
```

Both result in:
- Binary at `/usr/local/bin/llm-proxy` (or `~/.local/bin/`)
- Service running at login
- Shell env vars configured

---

## File Locations

| Purpose | macOS | Linux |
|---------|-------|-------|
| Binary | `/usr/local/bin/llm-proxy` | `/usr/local/bin/` or `~/.local/bin/` |
| Service | `~/Library/LaunchAgents/com.llm-proxy.plist` | `~/.config/systemd/user/llm-proxy.service` |
| State | `~/.local/state/llm-proxy/port` | `~/.local/state/llm-proxy/port` |
| Env script | `~/.config/llm-proxy/env.sh` | `~/.config/llm-proxy/env.sh` |
| Logs | `~/.llm-provider-logs/<host>/<date>/` | `~/.llm-provider-logs/<host>/<date>/` |

---

## Startup Flow

```
┌─────────────────────────────────────────────────────────────┐
│                    User Logs In                              │
└─────────────────────────────────────────────────────────────┘
                            │
           ┌────────────────┴────────────────┐
           ▼                                 ▼
┌─────────────────────┐           ┌─────────────────────┐
│  Service starts     │           │  Shell opens        │
│  (launchd/systemd)  │           │  (terminal/ssh)     │
└─────────────────────┘           └─────────────────────┘
           │                                 │
           ▼                                 ▼
┌─────────────────────┐           ┌─────────────────────┐
│  Proxy binds :0     │           │  Sources env.sh     │
│  Gets port 52847    │──────────▶│  Reads portfile     │
│  Writes to portfile │           │  Sets BASE_URLs     │
└─────────────────────┘           └─────────────────────┘
```

---

## Log Directory Structure

```
~/.llm-provider-logs/
├── api.anthropic.com/
│   └── 2026-01-14/
│       ├── 20260114-091523-a1b2c3d4.jsonl
│       └── 20260114-143052-e5f6g7h8.jsonl
├── api.openai.com/
│   └── 2026-01-14/
│       └── 20260114-102234-i9j0k1l2.jsonl
└── chatgpt.com/
    └── 2026-01-14/
        └── 20260114-111448-m3n4o5p6.jsonl
```

Host directory is the actual upstream after routing (OAuth requests show `chatgpt.com`).

---

## CLI Flags

```
llm-proxy                  # Run proxy (foreground, for testing)
llm-proxy --service        # Run as service (dynamic port, writes portfile)
llm-proxy --setup          # Full setup (service + shell, for Linux installer)
llm-proxy --setup-shell    # Shell only (for Homebrew post_install)
llm-proxy --uninstall      # Remove service and shell integration
llm-proxy --status         # Show if running, port, log location
```

---

## env.sh Script

```bash
# ~/.config/llm-proxy/env.sh
# Source this from your shell rc

_llm_proxy_port_file="$HOME/.local/state/llm-proxy/port"

if [ -f "$_llm_proxy_port_file" ]; then
    _port=$(cat "$_llm_proxy_port_file")
    if curl -sf "http://localhost:$_port/health" >/dev/null 2>&1; then
        export ANTHROPIC_BASE_URL="http://localhost:$_port/anthropic/api.anthropic.com"
        export OPENAI_BASE_URL="http://localhost:$_port/openai/api.openai.com"
    fi
fi
unset _llm_proxy_port_file _port
```

---

## Homebrew Formula

```ruby
class LlmProxy < Formula
  desc "Transparent logging proxy for LLM API traffic"
  homepage "https://github.com/obra/llm-proxy"
  version "0.1.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/obra/llm-proxy/releases/download/v#{version}/llm-proxy-darwin-arm64.tar.gz"
      sha256 "..."
    else
      url "https://github.com/obra/llm-proxy/releases/download/v#{version}/llm-proxy-darwin-amd64.tar.gz"
      sha256 "..."
    end
  end

  def install
    bin.install "llm-proxy"
  end

  service do
    run [opt_bin/"llm-proxy", "--service"]
    keep_alive true
    log_path var/"log/llm-proxy.log"
    error_log_path var/"log/llm-proxy.log"
  end

  def post_install
    system bin/"llm-proxy", "--setup-shell"
  end

  def caveats
    <<~EOS
      To start llm-proxy now and restart at login:
        brew services start llm-proxy

      Then restart your shell or run:
        source ~/.config/llm-proxy/env.sh
    EOS
  end
end
```

---

## Linux Install Script

```bash
#!/bin/sh
set -e

ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported: $ARCH"; exit 1 ;;
esac

URL="https://github.com/obra/llm-proxy/releases/download/latest/llm-proxy-linux-$ARCH"
curl -fsSL "$URL" -o /tmp/llm-proxy
chmod +x /tmp/llm-proxy

if [ -w /usr/local/bin ]; then
  mv /tmp/llm-proxy /usr/local/bin/
else
  mkdir -p ~/.local/bin
  mv /tmp/llm-proxy ~/.local/bin/
  export PATH="$HOME/.local/bin:$PATH"
fi

llm-proxy --setup
echo "Done! Restart your shell or: source ~/.config/llm-proxy/env.sh"
```

---

## systemd User Service

```ini
# ~/.config/systemd/user/llm-proxy.service
[Unit]
Description=LLM API Logging Proxy
After=default.target

[Service]
Type=simple
ExecStart=/usr/local/bin/llm-proxy --service
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

---

## Multi-User Support

Each user gets their own:
- systemd --user service instance
- Dynamic port (no conflicts)
- Portfile in their home directory
- Log directory in their home directory

No shared state, no coordination needed.
