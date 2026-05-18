package paramvalidators

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestThinkingValidatorAccepts(t *testing.T) {
	v := ThinkingValidator{}
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{"messages":[]}`},
		{name: "enabled", body: `{"thinking":{"type":"enabled"}}`},
		{name: "disabled", body: `{"thinking":{"type":"disabled"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, v.Validate(parseDocument(t, tt.body)))
		})
	}
}

func TestThinkingValidatorRejects(t *testing.T) {
	v := ThinkingValidator{}
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "wrapper not object", body: `{"thinking":"enabled"}`, wantErr: ErrThinkingShape},
		{name: "wrapper is array", body: `{"thinking":[]}`, wantErr: ErrThinkingShape},
		{name: "wrapper is bool", body: `{"thinking":true}`, wantErr: ErrThinkingShape},
		{name: "type missing", body: `{"thinking":{}}`, wantErr: ErrThinkingType},
		{name: "type is bool", body: `{"thinking":{"type":true}}`, wantErr: ErrThinkingType},
		{name: "type is unknown string", body: `{"thinking":{"type":"on"}}`, wantErr: ErrThinkingType},
		{name: "type is empty string", body: `{"thinking":{"type":""}}`, wantErr: ErrThinkingType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(parseDocument(t, tt.body))
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}
