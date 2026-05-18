package paramvalidators

import (
	"errors"
	"fmt"
)

// ErrThinkingShape covers the wrapper-level rejection. ErrThinkingType covers the inner
// type field (missing / not a string / not enabled|disabled).
var (
	ErrThinkingShape = errors.New("thinking: invalid wrapper shape")
	ErrThinkingType  = errors.New("thinking.type: must be \"enabled\" or \"disabled\"")
)

// ThinkingValidator enforces the Kimi-K2 thinking contract: an object with a single `type`
// field whose value is either "enabled" or "disabled". Any other shape was previously
// pass-through, which means a malformed payload could leak into the Jinja template via
// applyKimiRequestOverrides (which mirrors thinking.type into chat_template_kwargs.thinking
// for Kimi-K2.6). Reject early at the gateway boundary instead.
type ThinkingValidator struct{}

func (v ThinkingValidator) Validate(document map[string]any) error {
	raw, exists := document["thinking"]
	if !exists {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: must be an object", ErrThinkingShape)
	}
	typeRaw, hasType := obj["type"]
	if !hasType {
		return fmt.Errorf("%w: type is required", ErrThinkingType)
	}
	typeStr, ok := typeRaw.(string)
	if !ok {
		return fmt.Errorf("%w: type must be a string", ErrThinkingType)
	}
	if typeStr != "enabled" && typeStr != "disabled" {
		return fmt.Errorf("%w: got %q", ErrThinkingType, typeStr)
	}
	return nil
}
