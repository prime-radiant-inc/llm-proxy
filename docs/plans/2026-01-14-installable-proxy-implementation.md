# LLM Proxy: Installable Auto-Configuring Proxy - Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the proxy installable via `brew install` (macOS) or `curl | sh` (Linux), with automatic shell configuration so LLM traffic is logged transparently.

**Architecture:** System service runs the proxy on a dynamic port, writes port to state file, shell sources env script that reads port and sets `*_BASE_URL` vars.

**Tech Stack:** Go, launchd (macOS), systemd --user (Linux), bash/zsh shell integration

---

## Task 1: Rename Project to llm-proxy

**Files:**
- Modify: `go.mod`
- Modify: `main.go`
- Modify: `README.md`

**Step 1: Update go.mod module name**

Change module from `github.com/obra/transparent-agent-logger` to `github.com/obra/llm-proxy`.

**Step 2: Update main.go binary name references**

Update the startup log message and any references to "transparent-agent-logger" or "agent-logger".

**Step 3: Update .gitignore**

Replace `transparent-agent-logger` and `agent-logger` with `llm-proxy`.

**Step 4: Run tests to verify nothing broke**

Run: `go test ./...`
Expected: All tests pass

**Step 5: Commit**

```bash
git add -A
git commit -m "chore: rename project to llm-proxy"
```

---

## Task 2: Update Log Directory Structure

**Files:**
- Modify: `logger.go`
- Modify: `logger_test.go`

**Step 1: Write failing test for new log path structure**

```go
func TestLogPathStructure(t *testing.T) {
    tmpDir := t.TempDir()
    logger, _ := NewLogger(tmpDir)
    defer logger.Close()

    sessionID := "20260114-091523-abcd1234"
    upstream := "api.anthropic.com"

    logger.LogSessionStart(sessionID, "anthropic", upstream)

    // Wait for async write
    time.Sleep(50 * time.Millisecond)

    // Expect: tmpDir/api.anthropic.com/2026-01-14/20260114-091523-abcd1234.jsonl
    today := time.Now().Format("2006-01-02")
    expectedPath := filepath.Join(tmpDir, upstream, today, sessionID+".jsonl")

    if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
        t.Errorf("Expected log at %s", expectedPath)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v -run TestLogPathStructure`
Expected: FAIL - log file at wrong location

**Step 3: Update logger to use new path structure**

Modify `getLogPath()` in logger.go to return:
- `<baseDir>/<upstream>/<YYYY-MM-DD>/<sessionID>.jsonl`

Instead of:
- `<baseDir>/<provider>/<sessionID>.jsonl`

**Step 4: Update existing tests that depend on old path structure**

Fix `TestProxyLogsRequests` and other tests that check log file locations.

**Step 5: Run all tests**

Run: `go test ./...`
Expected: All tests pass

**Step 6: Commit**

```bash
git add -A
git commit -m "feat: restructure logs to ~/.llm-provider-logs/<host>/<date>/"
```

---

## Task 3: Add Dynamic Port and Portfile Support

**Files:**
- Create: `service.go`
- Create: `service_test.go`
- Modify: `main.go`

**Step 1: Write test for dynamic port binding**

```go
func TestDynamicPortBinding(t *testing.T) {
    // Bind to port 0, OS assigns available port
    listener, err := net.Listen("tcp", ":0")
    if err != nil {
        t.Fatalf("Failed to bind: %v", err)
    }
    defer listener.Close()

    port := listener.Addr().(*net.TCPAddr).Port
    if port == 0 {
        t.Error("Expected non-zero port")
    }
}
```

**Step 2: Write test for portfile writing**

```go
func TestWritePortfile(t *testing.T) {
    tmpDir := t.TempDir()
    portfile := filepath.Join(tmpDir, "port")

    err := WritePortfile(portfile, 52847)
    if err != nil {
        t.Fatalf("WritePortfile failed: %v", err)
    }

    data, _ := os.ReadFile(portfile)
    if string(data) != "52847" {
        t.Errorf("Expected '52847', got %q", data)
    }
}
```

**Step 3: Implement WritePortfile and ReadPortfile**

```go
// service.go
package main

import (
    "fmt"
    "os"
    "path/filepath"
    "strconv"
)

func DefaultPortfilePath() string {
    home, _ := os.UserHomeDir()
    return filepath.Join(home, ".local", "state", "llm-proxy", "port")
}

func WritePortfile(path string, port int) error {
    dir := filepath.Dir(path)
    if err := os.MkdirAll(dir, 0755); err != nil {
        return err
    }
    return os.WriteFile(path, []byte(strconv.Itoa(port)), 0644)
}

func ReadPortfile(path string) (int, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return 0, err
    }
    return strconv.Atoi(string(data))
}
```

**Step 4: Run tests**

Run: `go test -v -run "TestDynamicPort|TestWritePortfile"`
Expected: PASS

**Step 5: Commit**

```bash
git add service.go service_test.go
git commit -m "feat: add portfile support for dynamic port"
```

---

## Task 4: Add --service Flag

**Files:**
- Modify: `main.go`
- Modify: `config.go`
- Modify: `server.go`

**Step 1: Add --service flag to config**

Add `ServiceMode bool` to Config struct and parse `--service` flag.

**Step 2: Modify server startup for service mode**

When `--service` is true:
- Bind to `:0` (dynamic port)
- Write port to portfile
- Use `~/.llm-provider-logs/` as log directory

**Step 3: Test manually**

Run: `./llm-proxy --service &`
Check: `cat ~/.local/state/llm-proxy/port` shows a port number
Check: `curl http://localhost:$(cat ~/.local/state/llm-proxy/port)/health` returns "ok"

**Step 4: Commit**

```bash
git add -A
git commit -m "feat: add --service flag for daemon mode"
```

---

## Task 5: Create env.sh Generator

**Files:**
- Create: `setup.go`
- Create: `setup_test.go`
- Create: `templates/env.sh`

**Step 1: Write test for env.sh content**

```go
func TestGenerateEnvScript(t *testing.T) {
    script := GenerateEnvScript()

    if !strings.Contains(script, "ANTHROPIC_BASE_URL") {
        t.Error("Missing ANTHROPIC_BASE_URL")
    }
    if !strings.Contains(script, "OPENAI_BASE_URL") {
        t.Error("Missing OPENAI_BASE_URL")
    }
    if !strings.Contains(script, ".local/state/llm-proxy/port") {
        t.Error("Missing portfile path")
    }
}
```

**Step 2: Implement GenerateEnvScript**

```go
func GenerateEnvScript() string {
    return `# LLM Proxy environment configuration
# Source this from your shell rc file

_llm_proxy_port_file="$HOME/.local/state/llm-proxy/port"

if [ -f "$_llm_proxy_port_file" ]; then
    _llm_proxy_port=$(cat "$_llm_proxy_port_file")
    if curl -sf "http://localhost:$_llm_proxy_port/health" >/dev/null 2>&1; then
        export ANTHROPIC_BASE_URL="http://localhost:$_llm_proxy_port/anthropic/api.anthropic.com"
        export OPENAI_BASE_URL="http://localhost:$_llm_proxy_port/openai/api.openai.com"
    fi
fi
unset _llm_proxy_port_file _llm_proxy_port
`
}
```

**Step 3: Implement WriteEnvScript**

```go
func EnvScriptPath() string {
    home, _ := os.UserHomeDir()
    return filepath.Join(home, ".config", "llm-proxy", "env.sh")
}

func WriteEnvScript() error {
    path := EnvScriptPath()
    dir := filepath.Dir(path)
    if err := os.MkdirAll(dir, 0755); err != nil {
        return err
    }
    return os.WriteFile(path, []byte(GenerateEnvScript()), 0644)
}
```

**Step 4: Run tests**

Run: `go test -v -run TestGenerateEnvScript`
Expected: PASS

**Step 5: Commit**

```bash
git add setup.go setup_test.go
git commit -m "feat: add env.sh generator"
```

---

## Task 6: Add Shell RC Patching

**Files:**
- Modify: `setup.go`
- Modify: `setup_test.go`

**Step 1: Write test for shell rc patching**

```go
func TestPatchShellRC(t *testing.T) {
    tmpDir := t.TempDir()
    bashrc := filepath.Join(tmpDir, ".bashrc")

    // Create existing bashrc
    os.WriteFile(bashrc, []byte("# existing content\n"), 0644)

    err := PatchShellRC(bashrc, "/path/to/env.sh")
    if err != nil {
        t.Fatalf("PatchShellRC failed: %v", err)
    }

    content, _ := os.ReadFile(bashrc)
    if !strings.Contains(string(content), "source /path/to/env.sh") {
        t.Error("Missing source line")
    }
    if !strings.Contains(string(content), "# existing content") {
        t.Error("Clobbered existing content")
    }
}

func TestPatchShellRCIdempotent(t *testing.T) {
    tmpDir := t.TempDir()
    bashrc := filepath.Join(tmpDir, ".bashrc")

    os.WriteFile(bashrc, []byte("# existing\n"), 0644)

    PatchShellRC(bashrc, "/path/to/env.sh")
    PatchShellRC(bashrc, "/path/to/env.sh") // Second call

    content, _ := os.ReadFile(bashrc)
    count := strings.Count(string(content), "source /path/to/env.sh")
    if count != 1 {
        t.Errorf("Expected 1 source line, got %d", count)
    }
}
```

**Step 2: Implement PatchShellRC**

```go
const shellRCMarker = "# LLM Proxy"

func PatchShellRC(rcPath, envScriptPath string) error {
    content, err := os.ReadFile(rcPath)
    if err != nil && !os.IsNotExist(err) {
        return err
    }

    // Already patched?
    if strings.Contains(string(content), shellRCMarker) {
        return nil
    }

    line := fmt.Sprintf("\n%s\nsource %q\n", shellRCMarker, envScriptPath)

    f, err := os.OpenFile(rcPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return err
    }
    defer f.Close()

    _, err = f.WriteString(line)
    return err
}
```

**Step 3: Implement PatchAllShells**

```go
func PatchAllShells() error {
    home, _ := os.UserHomeDir()
    envScript := EnvScriptPath()

    shells := []string{".bashrc", ".zshrc"}
    for _, shell := range shells {
        rcPath := filepath.Join(home, shell)
        if _, err := os.Stat(rcPath); err == nil {
            if err := PatchShellRC(rcPath, envScript); err != nil {
                return err
            }
        }
    }
    return nil
}
```

**Step 4: Run tests**

Run: `go test -v -run "TestPatchShellRC"`
Expected: PASS

**Step 5: Commit**

```bash
git add -A
git commit -m "feat: add shell rc patching"
```

---

## Task 7: Add --setup-shell Flag

**Files:**
- Modify: `main.go`
- Modify: `config.go`

**Step 1: Add --setup-shell flag**

When `--setup-shell` is passed:
1. Call `WriteEnvScript()`
2. Call `PatchAllShells()`
3. Print success message and exit

**Step 2: Implement in main.go**

```go
if cfg.SetupShell {
    if err := WriteEnvScript(); err != nil {
        log.Fatalf("Failed to write env script: %v", err)
    }
    if err := PatchAllShells(); err != nil {
        log.Fatalf("Failed to patch shell rc: %v", err)
    }
    fmt.Println("Shell configuration complete.")
    fmt.Printf("Restart your shell or run: source %s\n", EnvScriptPath())
    os.Exit(0)
}
```

**Step 3: Test manually**

Run: `./llm-proxy --setup-shell`
Check: `~/.config/llm-proxy/env.sh` exists
Check: `~/.bashrc` or `~/.zshrc` contains source line

**Step 4: Commit**

```bash
git add -A
git commit -m "feat: add --setup-shell flag"
```

---

## Task 8: Add systemd Service Generator

**Files:**
- Modify: `setup.go`
- Modify: `setup_test.go`

**Step 1: Write test for systemd unit generation**

```go
func TestGenerateSystemdUnit(t *testing.T) {
    unit := GenerateSystemdUnit("/usr/local/bin/llm-proxy")

    if !strings.Contains(unit, "ExecStart=/usr/local/bin/llm-proxy --service") {
        t.Error("Missing ExecStart")
    }
    if !strings.Contains(unit, "[Install]") {
        t.Error("Missing Install section")
    }
}
```

**Step 2: Implement GenerateSystemdUnit**

```go
func GenerateSystemdUnit(binaryPath string) string {
    return fmt.Sprintf(`[Unit]
Description=LLM API Logging Proxy
After=default.target

[Service]
Type=simple
ExecStart=%s --service
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, binaryPath)
}
```

**Step 3: Implement InstallSystemdService**

```go
func SystemdServicePath() string {
    home, _ := os.UserHomeDir()
    return filepath.Join(home, ".config", "systemd", "user", "llm-proxy.service")
}

func InstallSystemdService(binaryPath string) error {
    path := SystemdServicePath()
    dir := filepath.Dir(path)
    if err := os.MkdirAll(dir, 0755); err != nil {
        return err
    }
    unit := GenerateSystemdUnit(binaryPath)
    return os.WriteFile(path, []byte(unit), 0644)
}
```

**Step 4: Run tests**

Run: `go test -v -run TestGenerateSystemdUnit`
Expected: PASS

**Step 5: Commit**

```bash
git add -A
git commit -m "feat: add systemd service generator"
```

---

## Task 9: Add --setup Flag (Full Linux Setup)

**Files:**
- Modify: `main.go`
- Modify: `config.go`
- Modify: `setup.go`

**Step 1: Implement FullSetup function**

```go
func FullSetup() error {
    // Find our binary path
    binaryPath, err := os.Executable()
    if err != nil {
        return err
    }

    // Install systemd service
    if err := InstallSystemdService(binaryPath); err != nil {
        return fmt.Errorf("failed to install service: %w", err)
    }

    // Enable and start service
    if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
        return fmt.Errorf("daemon-reload failed: %w", err)
    }
    if err := exec.Command("systemctl", "--user", "enable", "llm-proxy").Run(); err != nil {
        return fmt.Errorf("enable failed: %w", err)
    }
    if err := exec.Command("systemctl", "--user", "start", "llm-proxy").Run(); err != nil {
        return fmt.Errorf("start failed: %w", err)
    }

    // Setup shell
    if err := WriteEnvScript(); err != nil {
        return err
    }
    if err := PatchAllShells(); err != nil {
        return err
    }

    return nil
}
```

**Step 2: Add --setup flag handling in main.go**

```go
if cfg.Setup {
    if err := FullSetup(); err != nil {
        log.Fatalf("Setup failed: %v", err)
    }
    fmt.Println("LLM Proxy installed and started.")
    fmt.Printf("Restart your shell or run: source %s\n", EnvScriptPath())
    os.Exit(0)
}
```

**Step 3: Test manually on Linux**

Run: `./llm-proxy --setup`
Check: `systemctl --user status llm-proxy` shows running
Check: `curl http://localhost:$(cat ~/.local/state/llm-proxy/port)/health`

**Step 4: Commit**

```bash
git add -A
git commit -m "feat: add --setup flag for full Linux installation"
```

---

## Task 10: Add --uninstall Flag

**Files:**
- Modify: `setup.go`
- Modify: `main.go`
- Modify: `config.go`

**Step 1: Implement Uninstall function**

```go
func Uninstall() error {
    // Stop and disable systemd service (ignore errors if not installed)
    exec.Command("systemctl", "--user", "stop", "llm-proxy").Run()
    exec.Command("systemctl", "--user", "disable", "llm-proxy").Run()

    // Remove service file
    os.Remove(SystemdServicePath())

    // Remove env script
    os.Remove(EnvScriptPath())

    // Remove source lines from shell rc files
    if err := UnpatchAllShells(); err != nil {
        return err
    }

    // Remove portfile
    os.Remove(DefaultPortfilePath())

    fmt.Println("LLM Proxy uninstalled.")
    fmt.Println("Logs preserved at ~/.llm-provider-logs/")
    return nil
}

func UnpatchShellRC(rcPath string) error {
    content, err := os.ReadFile(rcPath)
    if err != nil {
        return nil // File doesn't exist, nothing to do
    }

    lines := strings.Split(string(content), "\n")
    var newLines []string
    skip := false
    for _, line := range lines {
        if strings.Contains(line, shellRCMarker) {
            skip = true
            continue
        }
        if skip && strings.HasPrefix(line, "source ") && strings.Contains(line, "llm-proxy") {
            skip = false
            continue
        }
        skip = false
        newLines = append(newLines, line)
    }

    return os.WriteFile(rcPath, []byte(strings.Join(newLines, "\n")), 0644)
}

func UnpatchAllShells() error {
    home, _ := os.UserHomeDir()
    for _, shell := range []string{".bashrc", ".zshrc"} {
        UnpatchShellRC(filepath.Join(home, shell))
    }
    return nil
}
```

**Step 2: Add --uninstall flag handling**

**Step 3: Commit**

```bash
git add -A
git commit -m "feat: add --uninstall flag"
```

---

## Task 11: Add --status Flag

**Files:**
- Modify: `main.go`
- Modify: `config.go`

**Step 1: Implement Status function**

```go
func Status() {
    portfile := DefaultPortfilePath()
    port, err := ReadPortfile(portfile)
    if err != nil {
        fmt.Println("Status: NOT RUNNING")
        fmt.Printf("Portfile: %s (not found)\n", portfile)
        return
    }

    // Check if actually responding
    resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", port))
    if err != nil || resp.StatusCode != 200 {
        fmt.Println("Status: NOT RUNNING (stale portfile)")
        fmt.Printf("Port: %d\n", port)
        return
    }

    home, _ := os.UserHomeDir()
    logDir := filepath.Join(home, ".llm-provider-logs")

    fmt.Println("Status: RUNNING")
    fmt.Printf("Port: %d\n", port)
    fmt.Printf("Logs: %s\n", logDir)
    fmt.Printf("Portfile: %s\n", portfile)
}
```

**Step 2: Add --status flag handling**

**Step 3: Commit**

```bash
git add -A
git commit -m "feat: add --status flag"
```

---

## Task 12: Create Linux Install Script

**Files:**
- Create: `scripts/install.sh`

**Step 1: Write install script**

```bash
#!/bin/sh
set -e

echo "Installing LLM Proxy..."

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')

# Download URL (update for real releases)
VERSION="${LLM_PROXY_VERSION:-latest}"
URL="https://github.com/obra/llm-proxy/releases/download/${VERSION}/llm-proxy-${OS}-${ARCH}"

echo "Downloading from $URL..."
curl -fsSL "$URL" -o /tmp/llm-proxy
chmod +x /tmp/llm-proxy

# Install binary
if [ -w /usr/local/bin ]; then
  echo "Installing to /usr/local/bin/llm-proxy"
  mv /tmp/llm-proxy /usr/local/bin/llm-proxy
else
  echo "Installing to ~/.local/bin/llm-proxy"
  mkdir -p "$HOME/.local/bin"
  mv /tmp/llm-proxy "$HOME/.local/bin/llm-proxy"
  export PATH="$HOME/.local/bin:$PATH"
fi

# Run setup
llm-proxy --setup

echo ""
echo "Installation complete!"
echo "Restart your shell or run: source ~/.config/llm-proxy/env.sh"
```

**Step 2: Commit**

```bash
git add scripts/install.sh
git commit -m "feat: add Linux install script"
```

---

## Task 13: Create Homebrew Formula

**Files:**
- Create: `Formula/llm-proxy.rb`

**Step 1: Write formula**

```ruby
class LlmProxy < Formula
  desc "Transparent logging proxy for LLM API traffic"
  homepage "https://github.com/obra/llm-proxy"
  version "0.1.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/obra/llm-proxy/releases/download/v#{version}/llm-proxy-darwin-arm64.tar.gz"
      sha256 "PLACEHOLDER"
    end
    on_intel do
      url "https://github.com/obra/llm-proxy/releases/download/v#{version}/llm-proxy-darwin-amd64.tar.gz"
      sha256 "PLACEHOLDER"
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

      Logs are stored in: ~/.llm-provider-logs/
    EOS
  end
end
```

**Step 2: Commit**

```bash
git add Formula/llm-proxy.rb
git commit -m "feat: add Homebrew formula"
```

---

## Task 14: Add Build Scripts for Releases

**Files:**
- Create: `scripts/build-release.sh`
- Modify: `Makefile` (create if needed)

**Step 1: Create build script**

```bash
#!/bin/bash
set -e

VERSION="${1:-dev}"
OUTDIR="dist"

mkdir -p "$OUTDIR"

# Build for all targets
for OS in darwin linux; do
  for ARCH in amd64 arm64; do
    echo "Building $OS/$ARCH..."
    GOOS=$OS GOARCH=$ARCH go build -ldflags="-s -w" -o "$OUTDIR/llm-proxy-$OS-$ARCH" .
  done
done

# Create tarballs for Homebrew
cd "$OUTDIR"
for f in llm-proxy-darwin-*; do
  tar -czf "${f}.tar.gz" "$f"
done

echo "Build complete. Artifacts in $OUTDIR/"
```

**Step 2: Create Makefile**

```makefile
.PHONY: build test release clean

build:
	go build -o llm-proxy .

test:
	go test ./...

release:
	./scripts/build-release.sh $(VERSION)

clean:
	rm -rf llm-proxy dist/
```

**Step 3: Commit**

```bash
git add scripts/build-release.sh Makefile
git commit -m "feat: add release build scripts"
```

---

## Task 15: Write README

**Files:**
- Modify: `README.md`

**Step 1: Write comprehensive README**

```markdown
# LLM Proxy

A transparent logging proxy for LLM API traffic. Install once, and every request to Claude, ChatGPT, or other LLM providers is automatically logged for debugging, auditing, and analysis.

## Quick Install

### macOS (Homebrew)

```bash
brew install obra/tap/llm-proxy
brew services start llm-proxy
```

### Linux

```bash
curl -fsSL https://llm-proxy.dev/install.sh | sh
```

Restart your shell, and you're done. All LLM traffic is now logged.

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
llm-proxy --setup       # Full setup (Linux only)
llm-proxy --setup-shell # Configure shell only
llm-proxy --uninstall   # Remove service and shell config
```

## How It Works

1. **Service runs in background** on a dynamic port
2. **Port is written** to `~/.local/state/llm-proxy/port`
3. **Shell sources** `~/.config/llm-proxy/env.sh` which reads the port
4. **Environment variables** like `ANTHROPIC_BASE_URL` point to the proxy
5. **Clients use the proxy** transparently—no client config needed

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

## License

MIT
```

**Step 2: Commit**

```bash
git add README.md
git commit -m "docs: write comprehensive README"
```

---

## Task 16: Final Testing

**Step 1: Run all tests**

Run: `go test ./...`
Expected: All pass

**Step 2: Test full flow on Linux**

```bash
go build -o llm-proxy .
./llm-proxy --setup
source ~/.config/llm-proxy/env.sh
echo $ANTHROPIC_BASE_URL  # Should show http://localhost:XXXXX/anthropic/...
curl $ANTHROPIC_BASE_URL/../health  # Should return "ok"
```

**Step 3: Test --uninstall**

```bash
./llm-proxy --uninstall
systemctl --user status llm-proxy  # Should be stopped/not found
```

**Step 4: Commit any fixes**

```bash
git add -A
git commit -m "test: verify full installation flow"
```
