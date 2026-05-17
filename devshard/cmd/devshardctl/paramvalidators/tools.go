package paramvalidators

import (
	"errors"
	"fmt"
)

// Tool-specific sentinels. Schema-walk rejections (depth/nodes/ref/enum/branch/size) flow
// through as the shared ErrSchema* values wrapped with a "tools[i].function.parameters:"
// prefix.
var (
	ErrToolsShape        = errors.New("tools: invalid array shape")
	ErrToolShape         = errors.New("tools[i]: invalid tool shape")
	ErrToolFunctionType  = errors.New("tools[i].type: must be \"function\"")
	ErrToolFunctionShape = errors.New("tools[i].function: invalid wrapper shape")
	ErrToolFunctionName  = errors.New("tools[i].function.name: must be a non-empty string")
)

// ToolsValidator enforces the OpenAI tool object contract -- every tool must declare
// `type: "function"` and a `function` object with a non-empty `name` -- and then bounds
// `function.parameters` via SchemaBounds (same grammar-compiler attack surface as
// response_format). Parameter-less tools are allowed by the OpenAI spec, so an absent
// `parameters` field is a no-op rather than a failure.
type ToolsValidator struct {
	MaxDepth      int
	MaxSize       int
	MaxNodes      int
	MaxBranch     int
	MaxEnum       int
	MaxPatternLen int
}

func (v ToolsValidator) Validate(document map[string]any) error {
	raw, exists := document["tools"]
	if !exists {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%w: must be an array", ErrToolsShape)
	}
	bounds := SchemaBounds{
		MaxDepth:      v.MaxDepth,
		MaxSize:       v.MaxSize,
		MaxNodes:      v.MaxNodes,
		MaxBranch:     v.MaxBranch,
		MaxEnum:       v.MaxEnum,
		MaxPatternLen: v.MaxPatternLen,
	}
	for i, item := range arr {
		tool, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: tools[%d] must be an object", ErrToolShape, i)
		}
		toolType, ok := tool["type"].(string)
		if !ok || toolType != "function" {
			return fmt.Errorf("%w (tools[%d])", ErrToolFunctionType, i)
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			return fmt.Errorf("%w: tools[%d].function must be an object", ErrToolFunctionShape, i)
		}
		name, ok := fn["name"].(string)
		if !ok || name == "" {
			return fmt.Errorf("%w (tools[%d])", ErrToolFunctionName, i)
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		if err := bounds.Walk(params); err != nil {
			return fmt.Errorf("tools[%d].function.parameters: %w", i, err)
		}
		if err := bounds.CheckSize(params); err != nil {
			return fmt.Errorf("tools[%d].function.parameters: %w", i, err)
		}
	}
	return nil
}
