package paramvalidators

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func defaultChatTemplateKwargsValidator() ChatTemplateKwargsValidator {
	return ChatTemplateKwargsValidator{
		MaxDepth: 5,
		MaxSize:  16 * 1024,
		MaxNodes: 128,
	}
}

func TestChatTemplateKwargsValidatorAccepts(t *testing.T) {
	v := defaultChatTemplateKwargsValidator()
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{"messages":[]}`},
		{name: "empty object", body: `{"chat_template_kwargs":{}}`},
		{name: "kimi thinking shape", body: `{"chat_template_kwargs":{"thinking":true}}`},
		{name: "qwen enable_thinking + preserve", body: `{"chat_template_kwargs":{"enable_thinking":true,"preserve_thinking":false}}`},
		{name: "nested at depth limit", body: `{"chat_template_kwargs":` + nestedObjectChain(5) + `}`},
		{name: "array of strings", body: `{"chat_template_kwargs":{"tags":["a","b","c"]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseDocument(t, tt.body)
			require.NoError(t, v.Validate(doc))
		})
	}
}

func TestChatTemplateKwargsValidatorRejects(t *testing.T) {
	v := defaultChatTemplateKwargsValidator()
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "wrapper not object", body: `{"chat_template_kwargs":"hi"}`, wantErr: ErrChatTemplateKwargsShape},
		{name: "wrapper is array", body: `{"chat_template_kwargs":[1,2]}`, wantErr: ErrChatTemplateKwargsShape},
		{name: "depth exceeds limit", body: `{"chat_template_kwargs":` + nestedObjectChain(6) + `}`, wantErr: ErrSchemaDepth},
		{name: "deep recursion attack", body: `{"chat_template_kwargs":` + nestedObjectChain(200) + `}`, wantErr: ErrSchemaDepth},
		{name: "node count exceeds limit", body: `{"chat_template_kwargs":` + flatPropertiesObject(200) + `}`, wantErr: ErrSchemaNodes},
		{name: "size exceeds limit", body: `{"chat_template_kwargs":{"x":"` + strings.Repeat("a", 17*1024) + `"}}`, wantErr: ErrSchemaSize},
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

// nestedObjectChain produces {"x": {"x": ... {} }} -- a plain-object chain (no JSON Schema).
func nestedObjectChain(depth int) string {
	if depth <= 1 {
		return `{}`
	}
	return `{"x":` + nestedObjectChain(depth-1) + `}`
}

// flatPropertiesObject is a single object with `count` keys, each mapped to `{}`.
// Used to exhaust the node-count budget without hitting the depth cap.
func flatPropertiesObject(count int) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"k`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":{}`)
	}
	b.WriteByte('}')
	return b.String()
}
