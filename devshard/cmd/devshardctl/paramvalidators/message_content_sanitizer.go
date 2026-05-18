package paramvalidators

import "fmt"

// MessageContentSanitizerValidator rejects requests whose `messages[].content` contains
// a tokenizer special-token literal. The same rejection rationale as
// ToolContentSanitizerValidator (see special_tokens.go) — `messages[].content` is
// rendered verbatim into the chat-template prompt, so an attacker who slips
// `<|im_end|>` into a user message can prematurely end the role and forge subsequent
// turns once vLLM's tokenizer encodes it as a single special token ID.
//
// Handles both content shapes the gateway already accepts:
//
//   - String content (`{"role":"user","content":"<text>"}`)
//   - Typed content parts (`{"role":"user","content":[{"type":"text","text":"<text>"}, …]}`)
//
// Non-string content fragments (assistant `tool_calls`, tool `tool_call_id`, etc.) are
// not scanned — they don't carry free-text and are validated by the existing message
// processor.
type MessageContentSanitizerValidator struct{}

func (v MessageContentSanitizerValidator) Validate(vctx ValidatorContext) error {
	raw, exists := vctx.Document["messages"]
	if !exists {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	for i, m := range arr {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, hasContent := msg["content"]
		if !hasContent {
			continue
		}
		switch c := content.(type) {
		case string:
			if containsSpecialToken(c) {
				return fmt.Errorf("messages[%d].content: %w", i, ErrSpecialTokenInContent)
			}
		case []any:
			for j, part := range c {
				partMap, ok := part.(map[string]any)
				if !ok {
					continue
				}
				text, ok := partMap["text"].(string)
				if !ok {
					continue
				}
				if containsSpecialToken(text) {
					return fmt.Errorf("messages[%d].content[%d].text: %w", i, j, ErrSpecialTokenInContent)
				}
			}
		}
	}
	return nil
}
