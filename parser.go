package main

import (
	"encoding/json"
)

type ParsedRequest struct {
	Model     string
	MaxTokens int
	System    string
	Messages  []ParsedMessage
	Raw       map[string]interface{}
}

type ParsedMessage struct {
	Role        string
	TextContent string
	Content     []ContentBlock
	Raw         map[string]interface{}
}

type ParsedResponse struct {
	Content    []ContentBlock
	Usage      UsageInfo
	StopReason string
	Raw        map[string]interface{}
}

type ContentBlock struct {
	Type      string
	Text      string
	Thinking  string
	ToolID    string
	ToolName  string
	ToolInput map[string]interface{}
	Raw       map[string]interface{}
}

type UsageInfo struct {
	InputTokens  int
	OutputTokens int
}

func ParseRequestBody(body string, host string) ParsedRequest {
	var raw map[string]interface{}
	if json.Unmarshal([]byte(body), &raw) != nil {
		return ParsedRequest{Raw: raw}
	}

	parsed := ParsedRequest{Raw: raw}

	if model, ok := raw["model"].(string); ok {
		parsed.Model = model
	}
	if maxTokens, ok := raw["max_tokens"].(float64); ok {
		parsed.MaxTokens = int(maxTokens)
	}
	if system, ok := raw["system"].(string); ok {
		parsed.System = system
	}

	if messages, ok := raw["messages"].([]interface{}); ok {
		for _, m := range messages {
			if msg, ok := m.(map[string]interface{}); ok {
				pm := ParsedMessage{Raw: msg}

				if role, ok := msg["role"].(string); ok {
					pm.Role = role
				}

				// Handle simple string content
				if content, ok := msg["content"].(string); ok {
					pm.TextContent = content
				}

				// Handle array content (tool results, etc)
				if content, ok := msg["content"].([]interface{}); ok {
					for _, c := range content {
						if block, ok := c.(map[string]interface{}); ok {
							pm.Content = append(pm.Content, parseContentBlock(block))
						}
					}
					// Set TextContent from first text block for convenience
					for _, cb := range pm.Content {
						if cb.Type == "text" && pm.TextContent == "" {
							pm.TextContent = cb.Text
						}
					}
				}

				parsed.Messages = append(parsed.Messages, pm)
			}
		}
	}

	return parsed
}

func ParseResponseBody(body string, host string) ParsedResponse {
	var raw map[string]interface{}
	if json.Unmarshal([]byte(body), &raw) != nil {
		return ParsedResponse{Raw: raw}
	}

	parsed := ParsedResponse{Raw: raw}

	if content, ok := raw["content"].([]interface{}); ok {
		for _, c := range content {
			if block, ok := c.(map[string]interface{}); ok {
				parsed.Content = append(parsed.Content, parseContentBlock(block))
			}
		}
	}

	if usage, ok := raw["usage"].(map[string]interface{}); ok {
		if in, ok := usage["input_tokens"].(float64); ok {
			parsed.Usage.InputTokens = int(in)
		}
		if out, ok := usage["output_tokens"].(float64); ok {
			parsed.Usage.OutputTokens = int(out)
		}
	}

	if stop, ok := raw["stop_reason"].(string); ok {
		parsed.StopReason = stop
	}

	return parsed
}

func parseContentBlock(block map[string]interface{}) ContentBlock {
	cb := ContentBlock{Raw: block}

	if t, ok := block["type"].(string); ok {
		cb.Type = t
	}

	switch cb.Type {
	case "text":
		if text, ok := block["text"].(string); ok {
			cb.Text = text
		}
	case "thinking":
		if thinking, ok := block["thinking"].(string); ok {
			cb.Thinking = thinking
		}
	case "tool_use":
		if id, ok := block["id"].(string); ok {
			cb.ToolID = id
		}
		if name, ok := block["name"].(string); ok {
			cb.ToolName = name
		}
		if input, ok := block["input"].(map[string]interface{}); ok {
			cb.ToolInput = input
		}
	case "tool_result":
		if id, ok := block["tool_use_id"].(string); ok {
			cb.ToolID = id
		}
		if content, ok := block["content"].(string); ok {
			cb.Text = content
		}
	}

	return cb
}
