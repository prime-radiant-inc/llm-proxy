# llm-proxy

> A transparent logging proxy for LLM API traffic: install once and every request/response to Claude, OpenAI, and other providers is logged for debugging, auditing, and analysis.

**Family:** infra · **Type:** tool · **Lifecycle:** production · **Owner:** obra

## What it does
Sits between LLM clients (Claude Code, Codex, API scripts) and provider APIs, logging every request and response to `~/.llm-provider-logs/` as JSONL. It auto-configures the shell (`ANTHROPIC_BASE_URL`/`OPENAI_BASE_URL`) and runs as a background service started at login. Supports Anthropic, OpenAI, Bedrock, and OpenAI-compatible APIs, optional real-time export to Grafana Loki, and api-token substitution.

## How it fits
- Depends on: — (standalone Go binary; go.mod has only external/AWS SDK dependencies).
- Used by: terminus (cloud-build AMI builds and runs it) and terminus-stockyard (guest VM image bakes the release binary); developer machines via Homebrew/install script.
- External: Anthropic, OpenAI, AWS Bedrock APIs; optional Grafana Loki push; SQLite local store; Homebrew tap for distribution.

## Runtime & data
- Runs: Local background service / systemd unit (Linux) or brew service (macOS); also baked into server AMIs.
- Data in: Proxied LLM HTTP traffic from clients.
- Data out: JSONL logs to disk, optional batched push to Loki.

<!-- Maintained by the maintaining-project-map skill. Do not hand-edit; regenerated. -->
