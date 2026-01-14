# OpenAI Session Tracking Implementation Notes

## Current State

We have a working transparent logging proxy that:
1. Extracts session IDs from Anthropic's `metadata.user_id` field (format: `user_..._session_<uuid>`)
2. Only logs conversation endpoints (`/v1/messages`), skips token counting/event logging
3. Threads requests with same client session ID together in single log files
4. Falls back to fingerprint-based tracking when no session ID provided

**Key files:**
- `fingerprint.go` - `ExtractClientSessionID()` extracts session from Anthropic requests
- `proxy.go` - `isConversationEndpoint()` determines what to log
- `session.go` - `GetOrCreateSession()` handles session correlation
- `db.go` - `client_session_id` column tracks client-provided IDs

## OpenAI Endpoints to Support

### Must Log (Conversation Endpoints)

| Endpoint | Session ID Sources | Notes |
|----------|-------------------|-------|
| `/v1/chat/completions` | `user` field, `metadata`, `X-Session-ID` header | Main chat API |
| `/v1/responses` | `conversation`, `previous_response_id`, `metadata` | New recommended API |
| `/v1/completions` | `user` field | Legacy, rarely used |
| `/v1/threads/{id}/messages` | Thread ID from URL path | Assistants API (deprecated Aug 2026) |
| `/v1/threads/{id}/runs` | Thread ID from URL path | Assistants API runs |

### Skip Logging

| Endpoint | Reason |
|----------|--------|
| `/v1/conversations` | CRUD operations only, not actual messages |
| `/v1/models` | Metadata |
| `/v1/embeddings` | Not conversational |
| `/v1/images/*` | Not conversational |
| `/v1/audio/*` | Not conversational |
| `/v1/files/*` | Not conversational |
| `/v1/realtime` | WebSocket, different architecture |

## Session ID Extraction Strategy for OpenAI

Priority order (check each, use first non-empty):

```
1. URL path thread ID (for /v1/threads/{id}/...)
2. Request body: conversation (Responses API)
3. Request body: previous_response_id (Responses API chaining)
4. Request body: metadata.session_id (custom field)
5. Request header: X-Session-ID
6. Request header: X-Client-Request-Id
7. Request body: user field
8. Fingerprint fallback (existing implementation)
```

## Request Body Examples

### Chat Completions (`/v1/chat/completions`)
```json
{
  "model": "gpt-4o",
  "messages": [...],
  "user": "user-12345",
  "metadata": {"session_id": "abc-123"}
}
```

### Responses API (`/v1/responses`)
```json
{
  "model": "gpt-4o",
  "input": [...],
  "conversation": "conv_abc123",
  "previous_response_id": "resp_xyz789",
  "store": true,
  "metadata": {"session_id": "..."}
}
```

### Threads/Messages (`/v1/threads/{thread_id}/messages`)
- Thread ID is in the URL path, not body
- Format: `thread_<base62_id>`

## Implementation Tasks

1. **Update `isConversationEndpoint()`** to include OpenAI endpoints
2. **Update `ExtractClientSessionID()`** to handle OpenAI providers
3. **Add header extraction** - need to pass headers to extraction function
4. **Add URL path extraction** for thread IDs
5. **Update tests** for all new extraction patterns
6. **Test with real OpenAI-compatible clients** (if available)

## Key Differences from Anthropic

| Aspect | Anthropic | OpenAI |
|--------|-----------|--------|
| Session ID location | Always `metadata.user_id` | Multiple sources |
| ID format | Structured `user_..._session_<uuid>` | Freeform or `conv_`/`thread_`/`resp_` prefixed |
| Headers used | None | `X-Session-ID`, `X-Client-Request-Id` |
| URL-based IDs | No | Yes (thread IDs) |
| Native conversation tracking | No | Yes (Responses API) |

## Response ID Tracking (Future Enhancement)

For Responses API, we could also track response IDs to build explicit chains:
- Response returns `"id": "resp_abc123"`
- Next request uses `"previous_response_id": "resp_abc123"`
- This creates explicit parent-child relationships

## ChatGPT OAuth vs API Key Authentication

Codex CLI (and possibly other clients) use **different endpoints** based on authentication type:

| Auth Type | Endpoint | Notes |
|-----------|----------|-------|
| API Key (`sk-...`) | `https://api.openai.com/v1` | Standard OpenAI API |
| ChatGPT OAuth (JWT) | `https://chatgpt.com/backend-api/codex` | ChatGPT backend |

### Proxy Configuration for ChatGPT OAuth

When using this proxy with codex CLI OAuth authentication, you need to configure **both** endpoints:

```bash
# For API key authentication
export OPENAI_BASE_URL=http://localhost:8080/openai/api.openai.com

# For ChatGPT OAuth authentication (in ~/.codex/config.toml)
chatgpt_base_url = "http://localhost:8080/openai/chatgpt.com/backend-api"
```

The proxy URL format `/{provider}/{upstream}/{path}` allows routing to either endpoint.

### ChatGPT Backend API Detection

The proxy detects ChatGPT backend API conversation endpoints via path patterns:
- `/backend-api/codex/v1/responses`
- `/backend-api/v1/responses`
- Any path starting with `/backend-api/` and ending with `/responses`

## Test Scenarios Needed

1. OpenAI Chat Completions with `user` field
2. OpenAI Chat Completions with `metadata.session_id`
3. OpenAI Chat Completions with `X-Session-ID` header
4. OpenAI Responses API with `conversation` field
5. OpenAI Responses API with `previous_response_id` chaining
6. OpenAI Threads API with thread ID in URL
7. Mixed: first request has no ID, continuation has ID (should still thread via fingerprint then switch)
8. Verify non-conversation endpoints are NOT logged
9. ChatGPT OAuth: Requests to `/backend-api/codex/v1/responses` should be logged
