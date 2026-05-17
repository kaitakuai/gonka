package paramvalidators

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Schema-bounds sentinels. Returned by SchemaBounds.Walk / SchemaBounds.CheckSize and by
// ObjectBounds.Walk. Callers wrap them with a field-path prefix (e.g.
// "response_format.json_schema.schema", "tools[3].function.parameters",
// "chat_template_kwargs") so the user-facing message points at the offending field while
// errors.Is keeps working for the rejection category.
var (
	ErrSchemaDepth  = errors.New("nesting depth exceeded")
	ErrSchemaNodes  = errors.New("node count exceeded")
	ErrSchemaSize   = errors.New("serialized size exceeded")
	ErrSchemaRef    = errors.New("schema reference keyword is forbidden")
	ErrSchemaEnum   = errors.New("enum size exceeded")
	ErrSchemaBranch = errors.New("schema branch arms exceeded")
)

// SchemaBounds enforces the structural bounds that keep a JSON-Schema payload from
// exploding vLLM's grammar compiler. It is the JSON-Schema-aware walker reused by both
// `response_format.json_schema.schema` and `tools[].function.parameters` (they hit the same
// upstream compiler path).
type SchemaBounds struct {
	MaxDepth  int
	MaxSize   int
	MaxNodes  int
	MaxBranch int // anyOf / oneOf / allOf array arms
	MaxEnum   int // enum entries
}

// Walk recursively traverses the schema, enforcing depth/nodes/branch/enum bounds and the
// $ref/$defs/definitions ban. Walking is done before json.Marshal so attacker-controlled
// deeply nested payloads bail out at the depth check (~O(MaxNodes)) instead of after a full
// O(input size) marshal pass.
func (b SchemaBounds) Walk(schema map[string]any) error {
	var nodes int
	return b.walk(schema, 1, &nodes)
}

// CheckSize marshals the schema and rejects when the serialized size exceeds MaxSize. Run
// AFTER Walk so depth/breadth attacks rejected by the walker never pay for the marshal.
// MaxSize=0 disables the check.
func (b SchemaBounds) CheckSize(schema map[string]any) error {
	if b.MaxSize <= 0 {
		return nil
	}
	serialized, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("cannot be serialized: %v", err)
	}
	if len(serialized) > b.MaxSize {
		return fmt.Errorf("%w: limit %d bytes", ErrSchemaSize, b.MaxSize)
	}
	return nil
}

func (b SchemaBounds) walk(schema any, depth int, nodes *int) error {
	if depth > b.MaxDepth {
		return fmt.Errorf("%w: limit %d", ErrSchemaDepth, b.MaxDepth)
	}
	obj, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	*nodes++
	if *nodes > b.MaxNodes {
		return fmt.Errorf("%w: limit %d", ErrSchemaNodes, b.MaxNodes)
	}
	for _, forbidden := range forbiddenSchemaKeys {
		if _, exists := obj[forbidden]; exists {
			return fmt.Errorf("%w: %q is not allowed", ErrSchemaRef, forbidden)
		}
	}
	if enum, ok := obj["enum"].([]any); ok && len(enum) > b.MaxEnum {
		return fmt.Errorf("%w: limit %d", ErrSchemaEnum, b.MaxEnum)
	}
	for _, branchKey := range branchSchemaKeys {
		if arr, ok := obj[branchKey].([]any); ok && len(arr) > b.MaxBranch {
			return fmt.Errorf("%w: %s limit %d", ErrSchemaBranch, branchKey, b.MaxBranch)
		}
	}
	for key, value := range obj {
		if _, isData := responseFormatDataKeys[key]; isData {
			continue
		}
		switch typed := value.(type) {
		case map[string]any:
			if _, isChildMap := responseFormatChildMapKeys[key]; isChildMap {
				for _, child := range typed {
					if err := b.walk(child, depth+1, nodes); err != nil {
						return err
					}
				}
			} else {
				if err := b.walk(typed, depth+1, nodes); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := b.walk(child, depth+1, nodes); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ObjectBounds enforces depth/nodes/size on an arbitrary JSON object that is NOT a JSON
// Schema -- it has no special data-carrier keys, no $ref ban, no anyOf/enum semantics. Used
// for fields like `chat_template_kwargs` that feed vLLM's Jinja template renderer: we only
// care about bounding total structure, not validating individual JSON Schema constructs.
type ObjectBounds struct {
	MaxDepth int
	MaxSize  int
	MaxNodes int
}

// Walk recursively traverses every object/array node. Returns ErrSchemaDepth/ErrSchemaNodes
// (sentinels are shared with SchemaBounds because the rejection class is the same).
func (b ObjectBounds) Walk(obj map[string]any) error {
	var nodes int
	return b.walk(obj, 1, &nodes)
}

// CheckSize behaves identically to SchemaBounds.CheckSize.
func (b ObjectBounds) CheckSize(obj map[string]any) error {
	if b.MaxSize <= 0 {
		return nil
	}
	serialized, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("cannot be serialized: %v", err)
	}
	if len(serialized) > b.MaxSize {
		return fmt.Errorf("%w: limit %d bytes", ErrSchemaSize, b.MaxSize)
	}
	return nil
}

func (b ObjectBounds) walk(value any, depth int, nodes *int) error {
	if depth > b.MaxDepth {
		return fmt.Errorf("%w: limit %d", ErrSchemaDepth, b.MaxDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		*nodes++
		if *nodes > b.MaxNodes {
			return fmt.Errorf("%w: limit %d", ErrSchemaNodes, b.MaxNodes)
		}
		for _, child := range typed {
			if err := b.walk(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := b.walk(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}
