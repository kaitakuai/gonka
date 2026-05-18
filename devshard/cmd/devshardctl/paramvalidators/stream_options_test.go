package paramvalidators

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamOptionsValidatorAccepts(t *testing.T) {
	v := StreamOptionsValidator{}
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{"messages":[]}`},
		{name: "include_usage true", body: `{"stream_options":{"include_usage":true}}`},
		{name: "include_usage false", body: `{"stream_options":{"include_usage":false}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, v.Validate(parseDocument(t, tt.body)))
		})
	}
}

func TestStreamOptionsValidatorRejects(t *testing.T) {
	v := StreamOptionsValidator{}
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "wrapper is string", body: `{"stream_options":"include"}`, wantErr: ErrStreamOptionsShape},
		{name: "wrapper is array", body: `{"stream_options":[]}`, wantErr: ErrStreamOptionsShape},
		{name: "wrapper is bool", body: `{"stream_options":true}`, wantErr: ErrStreamOptionsShape},
		{name: "wrapper is number", body: `{"stream_options":42}`, wantErr: ErrStreamOptionsShape},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(parseDocument(t, tt.body))
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

// continuous_usage_stats is the vLLM-project/vllm#9028 trigger; clients send it innocently
// (it's an OpenAI-documented stream option) and the upstream produces a broken counter.
// The gateway strips it so the rest of the payload still goes through.
func TestStreamOptionsValidatorStripsContinuousUsageStats(t *testing.T) {
	v := StreamOptionsValidator{}
	doc := parseDocument(t, `{"stream_options":{"include_usage":true,"continuous_usage_stats":true}}`)
	require.NoError(t, v.Validate(doc))

	so, ok := doc["stream_options"].(map[string]any)
	require.True(t, ok, "stream_options should survive when include_usage remains")
	require.Equal(t, true, so["include_usage"])
	require.NotContains(t, so, "continuous_usage_stats")
}

// Future-proofing: an unknown sub-field added by upstream vLLM is also stripped, but does
// not reject the whole request (clients carrying it for unrelated features keep working).
func TestStreamOptionsValidatorStripsUnknownSubField(t *testing.T) {
	v := StreamOptionsValidator{}
	doc := parseDocument(t, `{"stream_options":{"include_usage":true,"fancy_new_field":42}}`)
	require.NoError(t, v.Validate(doc))

	so := doc["stream_options"].(map[string]any)
	require.Equal(t, true, so["include_usage"])
	require.NotContains(t, so, "fancy_new_field")
}

// If the rewrite empties the object, the field is dropped from the document so it never
// reaches the upstream as a meaningless `{}`.
func TestStreamOptionsValidatorDropsEmptyObject(t *testing.T) {
	v := StreamOptionsValidator{}
	doc := parseDocument(t, `{"stream_options":{"continuous_usage_stats":true}}`)
	require.NoError(t, v.Validate(doc))
	require.NotContains(t, doc, "stream_options")
}

// Already-empty object is treated the same as the previous case (no include_usage to keep).
func TestStreamOptionsValidatorDropsEmptyInput(t *testing.T) {
	v := StreamOptionsValidator{}
	doc := parseDocument(t, `{"stream_options":{}}`)
	require.NoError(t, v.Validate(doc))
	require.NotContains(t, doc, "stream_options")
}
