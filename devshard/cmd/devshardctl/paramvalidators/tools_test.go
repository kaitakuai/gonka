package paramvalidators

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func defaultToolsValidator() ToolsValidator {
	return ToolsValidator{
		MaxDepth:  5,
		MaxSize:   16 * 1024,
		MaxNodes:  128,
		MaxBranch: 16,
		MaxEnum:   256,
	}
}

func toolWithParams(schema string) string {
	return `{"type":"function","function":{"name":"x","description":"x","parameters":` + schema + `}}`
}

func TestToolsValidatorAccepts(t *testing.T) {
	v := defaultToolsValidator()
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{"messages":[]}`},
		{name: "empty array", body: `{"tools":[]}`},
		{name: "tool without function", body: `{"tools":[{"type":"function"}]}`},
		{name: "tool without parameters", body: `{"tools":[{"type":"function","function":{"name":"x"}}]}`},
		{name: "simple parameters", body: `{"tools":[` + toolWithParams(`{"type":"object","properties":{"city":{"type":"string"}}}`) + `]}`},
		{name: "two tools both valid", body: `{"tools":[` + toolWithParams(`{"type":"object"}`) + `,` + toolWithParams(`{"type":"object","properties":{"x":{"type":"number"}}}`) + `]}`},
		{name: "parameters at depth limit", body: `{"tools":[` + toolWithParams(nestedPropertiesSchema(5)) + `]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseDocument(t, tt.body)
			require.NoError(t, v.Validate(doc))
		})
	}
}

func TestToolsValidatorRejects(t *testing.T) {
	v := defaultToolsValidator()
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "tools is object", body: `{"tools":{"x":1}}`, wantErr: ErrToolsShape},
		{name: "tools element is not object", body: `{"tools":["x"]}`, wantErr: ErrToolShape},
		{name: "depth exceeds limit", body: `{"tools":[` + toolWithParams(nestedPropertiesSchema(6)) + `]}`, wantErr: ErrSchemaDepth},
		{name: "deep recursion attack hidden in tool", body: `{"tools":[` + toolWithParams(nestedPropertiesSchema(200)) + `]}`, wantErr: ErrSchemaDepth},
		{name: "ref hidden in tool parameters", body: `{"tools":[` + toolWithParams(`{"$ref":"#/foo"}`) + `]}`, wantErr: ErrSchemaRef},
		{name: "ref hidden under if in tool", body: `{"tools":[` + toolWithParams(`{"if":{"$ref":"#/x"}}`) + `]}`, wantErr: ErrSchemaRef},
		{name: "node count exceeds in tool", body: `{"tools":[` + toolWithParams(manyPropertiesSchema(200)) + `]}`, wantErr: ErrSchemaNodes},
		{name: "size exceeds in tool", body: `{"tools":[` + toolWithParams(`{"type":"object","properties":{"`+strings.Repeat("a", 17*1024)+`":{"type":"string"}}}`) + `]}`, wantErr: ErrSchemaSize},
		{name: "anyOf exceeds in tool", body: `{"tools":[` + toolWithParams(`{"anyOf":[`+strings.Repeat(`{"type":"string"},`, 16)+`{"type":"string"}]}`) + `]}`, wantErr: ErrSchemaBranch},
		{name: "enum exceeds in tool", body: `{"tools":[` + toolWithParams(bigEnumSchema(257)) + `]}`, wantErr: ErrSchemaEnum},
		// Second tool is the bad one -- verifies we walk every element, not just the first.
		{name: "rejects bad schema in second tool", body: `{"tools":[` + toolWithParams(`{"type":"object"}`) + `,` + toolWithParams(`{"$ref":"#/x"}`) + `]}`, wantErr: ErrSchemaRef},
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
