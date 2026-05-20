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

type ThinkingValidator struct {
	MirrorToTemplateKwargsForModels []string
}

func (v ThinkingValidator) Validate(vctx ValidatorContext) error {
	raw, exists := vctx.Document["thinking"]
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
	if v.shouldMirror(vctx.RoutedModel) {
		return mirrorThinkingToTemplateKwargs(vctx.Document, typeStr == "enabled")
	}
	return nil
}

func (v ThinkingValidator) shouldMirror(routedModel string) bool {
	for _, m := range v.MirrorToTemplateKwargsForModels {
		if m == routedModel {
			return true
		}
	}
	return false
}

func mirrorThinkingToTemplateKwargs(document map[string]any, enabled bool) error {
	chatTemplateKwargs, err := getOrCreateChatTemplateKwargs(document)
	if err != nil {
		return err
	}
	if _, exists := chatTemplateKwargs["thinking"]; exists {
		return nil
	}
	chatTemplateKwargs["thinking"] = enabled
	return nil
}
