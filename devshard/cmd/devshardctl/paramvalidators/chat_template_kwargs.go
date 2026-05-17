package paramvalidators

import (
	"errors"
	"fmt"
)

// ErrChatTemplateKwargsShape covers the wrapper-level rejection: chat_template_kwargs must
// be a JSON object. Bounds rejections (depth/nodes/size) come back as the shared ErrSchema*
// sentinels via ObjectBounds.
var ErrChatTemplateKwargsShape = errors.New("chat_template_kwargs: invalid wrapper shape")

// ChatTemplateKwargsValidator bounds the depth/breadth/size of chat_template_kwargs before
// vLLM hands the object to Jinja's chat-template renderer. Unbounded structure can stall
// rendering and inflate memory on the inference node. Unlike response_format.json_schema,
// this is NOT a JSON Schema -- there is no $ref ban, no anyOf/enum semantics. Plain
// depth/nodes/size only.
type ChatTemplateKwargsValidator struct {
	MaxDepth int
	MaxSize  int
	MaxNodes int
}

func (v ChatTemplateKwargsValidator) Validate(document map[string]any) error {
	raw, exists := document["chat_template_kwargs"]
	if !exists {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: must be an object", ErrChatTemplateKwargsShape)
	}
	bounds := ObjectBounds{
		MaxDepth: v.MaxDepth,
		MaxSize:  v.MaxSize,
		MaxNodes: v.MaxNodes,
	}
	if err := bounds.Walk(obj); err != nil {
		return fmt.Errorf("chat_template_kwargs: %w", err)
	}
	if err := bounds.CheckSize(obj); err != nil {
		return fmt.Errorf("chat_template_kwargs: %w", err)
	}
	return nil
}
