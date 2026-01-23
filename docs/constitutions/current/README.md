# llm-proxy Constitution

This directory contains the architectural rules and patterns for the llm-proxy codebase. All code changes must adhere to these standards.

## Documents

| Document | Purpose |
|----------|---------|
| [architecture.md](architecture.md) | Layer boundaries, component responsibilities, data flow |
| [patterns.md](patterns.md) | Common patterns for error handling, concurrency, logging |
| [testing.md](testing.md) | Testing standards, coverage requirements, naming conventions |

## Quick Reference

### Layer Order
```
CLI → Server → Proxy/Session/Logger → Storage
```

### Key Rules

1. **Graceful degradation**: Secondary features (Loki) must not break primary features (proxying)
2. **Non-blocking async**: Use buffered channels, drop on full rather than block
3. **Config layering**: CLI flags > env vars > TOML > defaults
4. **Header obfuscation**: Always obfuscate sensitive headers before logging
5. **Test isolation**: Use `t.TempDir()`, clean up in defer blocks

### Adding Features

Before implementing, verify:
- [ ] Feature fits within layer boundaries
- [ ] Error handling follows graceful degradation
- [ ] Configuration follows layered loading pattern
- [ ] Tests cover happy path + error cases (80%+ coverage)
