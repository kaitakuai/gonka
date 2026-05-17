// Package paramvalidators hosts pure validators for individual chat-completion request fields.
// Each validator operates on the decoded request document (map[string]any) without any
// dependency on the main package's pipeline types, so it can be unit-tested in isolation and
// composed back into the catalog through a thin adapter.
package paramvalidators

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Sentinel errors for response_format rejection categories. Wrap them with fmt.Errorf("%w: ...")
// when adding parameter context (limit values, encountered values); callers can identify the
// rejection class via errors.Is even when the formatted message changes.
var (
	ErrResponseFormatShape       = errors.New("response_format: invalid wrapper shape")
	ErrResponseFormatType        = errors.New("response_format.type: missing or unsupported")
	ErrResponseFormatJSONSchema  = errors.New("response_format.json_schema: invalid wrapper shape")
	ErrResponseFormatName        = errors.New("response_format.json_schema.name: invalid")
	ErrResponseFormatSchemaShape = errors.New("response_format.json_schema.schema: invalid shape")
	ErrResponseFormatSize        = errors.New("response_format.json_schema.schema: serialized size exceeded")
	ErrResponseFormatDepth       = errors.New("response_format.json_schema.schema: nesting depth exceeded")
	ErrResponseFormatNodes       = errors.New("response_format.json_schema.schema: node count exceeded")
	ErrResponseFormatRef         = errors.New("response_format.json_schema.schema: schema reference keyword is forbidden")
	ErrResponseFormatEnum        = errors.New("response_format.json_schema.schema: enum size exceeded")
	ErrResponseFormatBranch      = errors.New("response_format.json_schema.schema: schema branch arms exceeded")
)

// ResponseFormatValidator bounds an OpenAI-compatible response_format payload before it is
// forwarded to vLLM. A pathological json_schema (deep recursion, huge byte size, runaway
// breadth, schema $refs) can crash the upstream grammar compiler, so any violation must
// reject the request before it leaves the gateway.
type ResponseFormatValidator struct {
	MaxDepth   int
	MaxSize    int
	MaxNodes   int
	MaxBranch  int
	MaxEnum    int
	MaxNameLen int
}

var responseFormatNameRegex = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// forbiddenSchemaKeys and branchSchemaKeys are walked once per node. Defining them at package
// scope keeps the slice headers off the per-call allocation path (the literal-in-range form
// allocates a fresh backing array on every walkSchema invocation).
var forbiddenSchemaKeys = []string{"$ref", "$defs", "definitions"}
var branchSchemaKeys = []string{"anyOf", "oneOf", "allOf"}

// responseFormatDataKeys lists JSON-Schema keywords whose values are *literal data*, not
// child schemas. They must NOT be recursed into; an attacker could otherwise put a deeply
// nested object inside `default`/`examples`/`const` and have it counted against the schema
// budget needlessly, or worse, hide structure the walker treats as schema-shaped.
var responseFormatDataKeys = map[string]struct{}{
	"enum":              {},
	"const":             {},
	"default":           {},
	"examples":          {},
	"required":          {},
	"dependentRequired": {},
}

// responseFormatChildMapKeys lists keywords whose values are *maps* of name->schema (not a
// schema themselves). We recurse into each map value as a separate child schema; the wrapper
// map itself is not counted as a schema node.
var responseFormatChildMapKeys = map[string]struct{}{
	"properties":        {},
	"patternProperties": {},
	"dependentSchemas":  {},
}

// Validate inspects the "response_format" entry of the given document. Returns nil if
// response_format is absent, has type text/json_object, or has a json_schema payload that
// fits within all configured bounds.
func (v ResponseFormatValidator) Validate(document map[string]any) error {
	raw, exists := document["response_format"]
	if !exists {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: must be an object", ErrResponseFormatShape)
	}
	typeValue, ok := obj["type"].(string)
	if !ok || strings.TrimSpace(typeValue) == "" {
		return fmt.Errorf("%w: must be a non-empty string", ErrResponseFormatType)
	}
	switch typeValue {
	case "text", "json_object":
		return nil
	case "json_schema":
		return v.validateJSONSchemaWrapper(obj)
	default:
		return fmt.Errorf("%w: %q is not supported (allowed: text, json_object, json_schema)", ErrResponseFormatType, typeValue)
	}
}

func (v ResponseFormatValidator) validateJSONSchemaWrapper(rf map[string]any) error {
	wrapper, ok := rf["json_schema"].(map[string]any)
	if !ok {
		return fmt.Errorf("%w: must be an object", ErrResponseFormatJSONSchema)
	}
	name, ok := wrapper["name"].(string)
	if !ok || name == "" {
		return fmt.Errorf("%w: must be a non-empty string", ErrResponseFormatName)
	}
	if len(name) > v.MaxNameLen {
		return fmt.Errorf("%w: must be %d characters or fewer", ErrResponseFormatName, v.MaxNameLen)
	}
	if !responseFormatNameRegex.MatchString(name) {
		return fmt.Errorf("%w: must match %s", ErrResponseFormatName, responseFormatNameRegex.String())
	}
	schema, ok := wrapper["schema"].(map[string]any)
	if !ok {
		return fmt.Errorf("%w: must be an object", ErrResponseFormatSchemaShape)
	}
	// Walk first so depth/nodes/breadth attacks bail out without ever paying for json.Marshal.
	// json.Marshal is O(input size) and would otherwise serialize attacker-controlled depth-200
	// payloads in full before we ever reach the depth check.
	var nodes int
	if err := v.walkSchema(schema, 1, &nodes); err != nil {
		return err
	}
	serialized, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("%w: cannot be serialized: %v", ErrResponseFormatSchemaShape, err)
	}
	if len(serialized) > v.MaxSize {
		return fmt.Errorf("%w: serialized size exceeds %d bytes", ErrResponseFormatSize, v.MaxSize)
	}
	return nil
}

// walkSchema visits every schema-shaped child of `schema` and applies depth/node/breadth
// caps and the $ref/$defs/definitions ban. The policy is inverted: instead of an allow-list
// of JSON-Schema keywords to descend into (which silently grew gaps with every new keyword
// like `if`/`then`/`contains`/`unevaluatedProperties`/...), we walk EVERY object-valued or
// array-of-object-valued field, with two narrow exceptions:
//
//   - responseFormatDataKeys (enum/const/default/examples/required/dependentRequired):
//     values are literal data, never schemas. Skipped entirely so attacker-controlled JSON
//     hidden under them is not chased through.
//   - responseFormatChildMapKeys (properties/patternProperties/dependentSchemas): values are
//     maps of name->schema. We descend into each map value as its own child schema rather
//     than treating the wrapper map as a schema.
//
// Unknown JSON keywords with object/array-of-object values are treated as if they could
// contain schemas. That over-walks slightly (e.g. an attacker's invented key `{"foo": {...}}`
// costs us one extra node) but never under-walks, which is the property that matters for
// safely bounding vLLM's grammar compiler. All budgets remain bounded.
func (v ResponseFormatValidator) walkSchema(schema any, depth int, nodes *int) error {
	if depth > v.MaxDepth {
		return fmt.Errorf("%w: limit %d", ErrResponseFormatDepth, v.MaxDepth)
	}
	obj, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	*nodes++
	if *nodes > v.MaxNodes {
		return fmt.Errorf("%w: limit %d", ErrResponseFormatNodes, v.MaxNodes)
	}
	for _, forbidden := range forbiddenSchemaKeys {
		if _, exists := obj[forbidden]; exists {
			return fmt.Errorf("%w: %q is not allowed", ErrResponseFormatRef, forbidden)
		}
	}
	if enum, ok := obj["enum"].([]any); ok && len(enum) > v.MaxEnum {
		return fmt.Errorf("%w: limit %d", ErrResponseFormatEnum, v.MaxEnum)
	}
	for _, branchKey := range branchSchemaKeys {
		if arr, ok := obj[branchKey].([]any); ok && len(arr) > v.MaxBranch {
			return fmt.Errorf("%w: %s limit %d", ErrResponseFormatBranch, branchKey, v.MaxBranch)
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
					if err := v.walkSchema(child, depth+1, nodes); err != nil {
						return err
					}
				}
			} else {
				if err := v.walkSchema(typed, depth+1, nodes); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := v.walkSchema(child, depth+1, nodes); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
