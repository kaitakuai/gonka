package paramvalidators

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func defaultResponseFormatValidator() ResponseFormatValidator {
	return ResponseFormatValidator{
		MaxDepth:      5,
		MaxSize:       16 * 1024,
		MaxNodes:      128,
		MaxBranch:     16,
		MaxEnum:       256,
		MaxNameLen:    64,
		MaxPatternLen: 512,
	}
}

func parseDocument(tb testing.TB, body string) map[string]any {
	tb.Helper()
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		tb.Fatalf("parse: %v", err)
	}
	return raw
}

func TestResponseFormatValidatorAccepts(t *testing.T) {
	v := defaultResponseFormatValidator()
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{"messages":[]}`},
		{name: "type text", body: `{"response_format":{"type":"text"}}`},
		{name: "type json_object", body: `{"response_format":{"type":"json_object"}}`},
		{name: "json_schema simple", body: `{"response_format":{"type":"json_schema","json_schema":{"name":"weather_v1","schema":{"type":"object","properties":{"city":{"type":"string"},"temp":{"type":"number"}},"required":["city"]}}}}`},
		{name: "json_schema at depth limit", body: jsonSchemaResponseFormatBody(nestedPropertiesSchema(5))},
		{name: "json_schema with anyOf at branch limit", body: jsonSchemaResponseFormatBody(`{"anyOf":[` + strings.Repeat(`{"type":"string"},`, 15) + `{"type":"string"}]}`)},
		{name: "json_schema with enum at limit", body: jsonSchemaResponseFormatBody(bigEnumSchema(256))},
		{name: "json_schema name with dots dashes underscores", body: `{"response_format":{"type":"json_schema","json_schema":{"name":"abc_DEF-1.2","schema":{"type":"object"}}}}`},
		// type can be an array of primitives (JSON Schema draft 6+); accept that shape.
		{name: "schema type as array of primitives", body: jsonSchemaResponseFormatBody(`{"type":["string","null"]}`)},
		// A reasonable regex pattern under the length cap compiles and is accepted.
		{name: "schema with valid pattern", body: jsonSchemaResponseFormatBody(`{"type":"string","pattern":"^[a-zA-Z0-9_-]+$"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseDocument(t, tt.body)
			require.NoError(t, v.Validate(doc))
		})
	}
}

func TestResponseFormatValidatorRejects(t *testing.T) {
	v := defaultResponseFormatValidator()
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "response_format not an object", body: `{"response_format":"hi"}`, wantErr: ErrResponseFormatShape},
		{name: "type missing", body: `{"response_format":{"json_schema":{"name":"r","schema":{"type":"object"}}}}`, wantErr: ErrResponseFormatType},
		{name: "type empty string", body: `{"response_format":{"type":""}}`, wantErr: ErrResponseFormatType},
		{name: "unknown type", body: `{"response_format":{"type":"banana"}}`, wantErr: ErrResponseFormatType},
		{name: "json_schema wrapper missing", body: `{"response_format":{"type":"json_schema"}}`, wantErr: ErrResponseFormatJSONSchema},
		{name: "json_schema missing name", body: `{"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}}`, wantErr: ErrResponseFormatName},
		{name: "json_schema name has bad chars", body: `{"response_format":{"type":"json_schema","json_schema":{"name":"bad name","schema":{"type":"object"}}}}`, wantErr: ErrResponseFormatName},
		{name: "json_schema name too long", body: `{"response_format":{"type":"json_schema","json_schema":{"name":"` + strings.Repeat("a", 65) + `","schema":{"type":"object"}}}}`, wantErr: ErrResponseFormatName},
		{name: "json_schema missing schema", body: `{"response_format":{"type":"json_schema","json_schema":{"name":"r"}}}`, wantErr: ErrResponseFormatSchemaShape},
		{name: "schema not an object", body: `{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":"x"}}}`, wantErr: ErrResponseFormatSchemaShape},
		{name: "depth exceeds limit", body: jsonSchemaResponseFormatBody(nestedPropertiesSchema(6)), wantErr: ErrResponseFormatDepth},
		{name: "deep recursion attack", body: jsonSchemaResponseFormatBody(nestedPropertiesSchema(200)), wantErr: ErrResponseFormatDepth},
		{name: "schema size exceeds 16 KiB", body: jsonSchemaResponseFormatBody(`{"type":"object","properties":{"` + strings.Repeat("a", 17*1024) + `":{"type":"string"}}}`), wantErr: ErrResponseFormatSize},
		{name: "schema node count exceeds 128", body: jsonSchemaResponseFormatBody(manyPropertiesSchema(200)), wantErr: ErrResponseFormatNodes},
		{name: "ref not allowed", body: jsonSchemaResponseFormatBody(`{"$ref":"#/foo"}`), wantErr: ErrResponseFormatRef},
		{name: "defs not allowed", body: jsonSchemaResponseFormatBody(`{"$defs":{"x":{}}}`), wantErr: ErrResponseFormatRef},
		{name: "definitions not allowed", body: jsonSchemaResponseFormatBody(`{"definitions":{"x":{}}}`), wantErr: ErrResponseFormatRef},
		{name: "anyOf exceeds branch limit", body: jsonSchemaResponseFormatBody(`{"anyOf":[` + strings.Repeat(`{"type":"string"},`, 16) + `{"type":"string"}]}`), wantErr: ErrResponseFormatBranch},
		{name: "oneOf exceeds branch limit", body: jsonSchemaResponseFormatBody(`{"oneOf":[` + strings.Repeat(`{"type":"string"},`, 16) + `{"type":"string"}]}`), wantErr: ErrResponseFormatBranch},
		{name: "allOf exceeds branch limit", body: jsonSchemaResponseFormatBody(`{"allOf":[` + strings.Repeat(`{"type":"string"},`, 16) + `{"type":"string"}]}`), wantErr: ErrResponseFormatBranch},
		{name: "enum exceeds limit", body: jsonSchemaResponseFormatBody(bigEnumSchema(257)), wantErr: ErrResponseFormatEnum},
		{name: "ref deep inside properties", body: jsonSchemaResponseFormatBody(`{"type":"object","properties":{"x":{"$ref":"#/y"}}}`), wantErr: ErrResponseFormatRef},
		// Regression: walker must traverse every schema-valued keyword. Hiding a deep nest or a
		// $ref under if/then/else/contains/not/propertyNames/unevaluated*/dependentSchemas
		// previously bypassed the validator.
		{name: "depth via if chain", body: jsonSchemaResponseFormatBody(nestedIfSchema(200)), wantErr: ErrResponseFormatDepth},
		{name: "depth via then chain", body: jsonSchemaResponseFormatBody(nestedKeywordSchema("then", 200)), wantErr: ErrResponseFormatDepth},
		{name: "depth via else chain", body: jsonSchemaResponseFormatBody(nestedKeywordSchema("else", 200)), wantErr: ErrResponseFormatDepth},
		{name: "depth via contains chain", body: jsonSchemaResponseFormatBody(nestedKeywordSchema("contains", 200)), wantErr: ErrResponseFormatDepth},
		{name: "depth via not chain", body: jsonSchemaResponseFormatBody(nestedKeywordSchema("not", 200)), wantErr: ErrResponseFormatDepth},
		{name: "depth via propertyNames chain", body: jsonSchemaResponseFormatBody(nestedKeywordSchema("propertyNames", 200)), wantErr: ErrResponseFormatDepth},
		{name: "depth via unevaluatedProperties chain", body: jsonSchemaResponseFormatBody(nestedKeywordSchema("unevaluatedProperties", 200)), wantErr: ErrResponseFormatDepth},
		{name: "ref hidden under if", body: jsonSchemaResponseFormatBody(`{"if":{"$ref":"#/x"}}`), wantErr: ErrResponseFormatRef},
		{name: "ref hidden under contains", body: jsonSchemaResponseFormatBody(`{"contains":{"$ref":"#/x"}}`), wantErr: ErrResponseFormatRef},
		{name: "ref hidden under not", body: jsonSchemaResponseFormatBody(`{"not":{"$ref":"#/x"}}`), wantErr: ErrResponseFormatRef},
		{name: "ref hidden under dependentSchemas", body: jsonSchemaResponseFormatBody(`{"dependentSchemas":{"x":{"$ref":"#/y"}}}`), wantErr: ErrResponseFormatRef},
		{name: "defs hidden under then", body: jsonSchemaResponseFormatBody(`{"then":{"$defs":{"x":{}}}}`), wantErr: ErrResponseFormatRef},
		// CVE-2025-48944: bad `type` value crashes xgrammar's C++ grammar compiler.
		{name: "bad schema type string", body: jsonSchemaResponseFormatBody(`{"type":"something"}`), wantErr: ErrSchemaType},
		{name: "bad schema type array entry", body: jsonSchemaResponseFormatBody(`{"type":["string","weird"]}`), wantErr: ErrSchemaType},
		{name: "bad schema type bool", body: jsonSchemaResponseFormatBody(`{"type":true}`), wantErr: ErrSchemaType},
		{name: "bad schema type nested", body: jsonSchemaResponseFormatBody(`{"type":"object","properties":{"x":{"type":"not_a_type"}}}`), wantErr: ErrSchemaType},
		// CVE-2025-48944: unclosed regex crashes the regex compiler before vLLM rejects.
		{name: "bad pattern unclosed group", body: jsonSchemaResponseFormatBody(`{"type":"string","pattern":"("}`), wantErr: ErrSchemaPattern},
		{name: "bad pattern unclosed char class", body: jsonSchemaResponseFormatBody(`{"type":"string","pattern":"["}`), wantErr: ErrSchemaPattern},
		{name: "bad pattern not string", body: jsonSchemaResponseFormatBody(`{"type":"string","pattern":42}`), wantErr: ErrSchemaPattern},
		{name: "bad pattern too long", body: jsonSchemaResponseFormatBody(`{"type":"string","pattern":"` + strings.Repeat("a", 513) + `"}`), wantErr: ErrSchemaPattern},
		{name: "bad pattern nested", body: jsonSchemaResponseFormatBody(`{"type":"object","properties":{"x":{"type":"string","pattern":"["}}}`), wantErr: ErrSchemaPattern},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseDocument(t, tt.body)
			err := v.Validate(doc)
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func jsonSchemaResponseFormatBody(schemaJSON string) string {
	return `{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` + schemaJSON + `}}}`
}

func nestedPropertiesSchema(depth int) string {
	if depth <= 1 {
		return `{"type":"object"}`
	}
	return `{"type":"object","properties":{"x":` + nestedPropertiesSchema(depth-1) + `}}`
}

func nestedIfSchema(depth int) string {
	return nestedKeywordSchema("if", depth)
}

// nestedKeywordSchema produces a chain of `depth` schemas nested through the given JSON-Schema
// keyword. Used to assert the walker enters every schema-valued keyword, not just the ones
// it explicitly recognized in its early implementation.
func nestedKeywordSchema(keyword string, depth int) string {
	if depth <= 1 {
		return `{"type":"object"}`
	}
	return `{"` + keyword + `":` + nestedKeywordSchema(keyword, depth-1) + `}`
}

func manyPropertiesSchema(count int) string {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"k`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":{}`)
	}
	b.WriteString(`}}`)
	return b.String()
}

func bigEnumSchema(n int) string {
	var b strings.Builder
	b.WriteString(`{"enum":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteString(`]}`)
	return b.String()
}
