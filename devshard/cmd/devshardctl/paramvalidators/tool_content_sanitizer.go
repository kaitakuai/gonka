package paramvalidators

import "fmt"

// ToolContentSanitizerValidator rejects requests whose `tools[]` entries carry tokenizer
// special-token literals in any field that vLLM's chat template will render into the
// prompt — `function.name`, `function.description`, every property name, and every
// nested string value inside `function.parameters` (including property descriptions,
// enum values, format hints, etc.).
//
// Complements the schema-shape walker in ToolsValidator (which bounds depth/nodes/size)
// with a content-level check: even a well-formed, small tool definition can be a
// prompt-injection vehicle if it carries `<|im_assistant|>` in its description.
//
// See special_tokens.go for the pattern and the rejection rationale.
type ToolContentSanitizerValidator struct{}

func (v ToolContentSanitizerValidator) Validate(vctx ValidatorContext) error {
	raw, exists := vctx.Document["tools"]
	if !exists {
		return nil
	}
	tools, ok := raw.([]any)
	if !ok {
		// Shape rejection is owned by ToolsValidator; let it speak.
		return nil
	}
	for i, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok && containsSpecialToken(name) {
			return fmt.Errorf("tools[%d].function.name: %w", i, ErrSpecialTokenInContent)
		}
		if desc, ok := fn["description"].(string); ok && containsSpecialToken(desc) {
			return fmt.Errorf("tools[%d].function.description: %w", i, ErrSpecialTokenInContent)
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		if err := walkSpecialTokensInObject(params, fmt.Sprintf("tools[%d].function.parameters", i)); err != nil {
			return err
		}
	}
	return nil
}

// walkSpecialTokensInObject scans every key and every string-valued leaf of a JSON
// object (recursively descending through nested objects and arrays) for tokenizer
// special-token literals.
func walkSpecialTokensInObject(obj map[string]any, path string) error {
	for key, value := range obj {
		if containsSpecialToken(key) {
			return fmt.Errorf("%s: key %q: %w", path, key, ErrSpecialTokenInContent)
		}
		childPath := path + "." + key
		if err := walkSpecialTokensInValue(value, childPath); err != nil {
			return err
		}
	}
	return nil
}

func walkSpecialTokensInValue(value any, path string) error {
	switch v := value.(type) {
	case string:
		if containsSpecialToken(v) {
			return fmt.Errorf("%s: %w", path, ErrSpecialTokenInContent)
		}
	case map[string]any:
		return walkSpecialTokensInObject(v, path)
	case []any:
		for i, item := range v {
			if err := walkSpecialTokensInValue(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
