package paramvalidators

import (
	"errors"
	"fmt"
)

// Tool-specific sentinels. Schema-walk rejections (depth/nodes/ref/enum/branch/size) flow
// through as the shared ErrSchema* values wrapped with a "tools[i].function.parameters:"
// prefix.
var (
	ErrToolsShape       = errors.New("tools: invalid array shape")
	ErrToolShape        = errors.New("tools[i]: invalid tool shape")
	ErrToolFunctionType = errors.New("tools[i].type: must be \"function\"")
)

// ToolsValidator applies SchemaBounds to every `tools[].function.parameters` schema.
//
// vLLM compiles tool argument schemas through the same grammar path as response_format,
// so the same depth/nodes/branch/enum/size/$ref invariants apply. Without this check, a
// recursive schema posted inside a tool definition would reach vLLM unbounded.
//
// Empty or absent tools is a no-op (the catalog also strips empty `tools` arrays
// elsewhere). Tools whose `function.parameters` field is absent or non-object are
// skipped -- they are nominally allowed by the OpenAI spec (parameter-less tools).
type ToolsValidator struct {
	MaxDepth  int
	MaxSize   int
	MaxNodes  int
	MaxBranch int
	MaxEnum   int
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
		MaxDepth:  v.MaxDepth,
		MaxSize:   v.MaxSize,
		MaxNodes:  v.MaxNodes,
		MaxBranch: v.MaxBranch,
		MaxEnum:   v.MaxEnum,
	}
	for i, item := range arr {
		tool, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: tools[%d] must be an object", ErrToolShape, i)
		}
		// We do not enforce `type: "function"` here -- the OpenAI tool spec allows future
		// expansion. We only validate the schema if it exists in the expected place.
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			// A tool without a function payload reaches vLLM as a no-op; nothing to bound.
			continue
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			// Parameter-less tools (no JSON Schema) -- valid per OpenAI spec.
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
