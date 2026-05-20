package paramvalidators

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafetyIdentifierValidatorAccepts(t *testing.T) {
	v := SafetyIdentifierValidator{}
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{"messages":[]}`},
		{name: "empty string", body: `{"safety_identifier":""}`},
		{name: "hashed user id", body: `{"safety_identifier":"sha256:abc123"}`},
		{name: "uuid", body: `{"safety_identifier":"550e8400-e29b-41d4-a716-446655440000"}`},
		{name: "exactly at length", body: `{"safety_identifier":"` + strings.Repeat("x", 512) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, v.Validate(ValidatorContext{Document: parseDocument(t, tt.body)}))
		})
	}
}

func TestSafetyIdentifierValidatorRejects(t *testing.T) {
	v := SafetyIdentifierValidator{}
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "number", body: `{"safety_identifier":42}`, wantErr: ErrSafetyIdentifierShape},
		{name: "boolean", body: `{"safety_identifier":true}`, wantErr: ErrSafetyIdentifierShape},
		{name: "object", body: `{"safety_identifier":{}}`, wantErr: ErrSafetyIdentifierShape},
		{name: "array", body: `{"safety_identifier":[]}`, wantErr: ErrSafetyIdentifierShape},
		{name: "null", body: `{"safety_identifier":null}`, wantErr: ErrSafetyIdentifierShape},
		{name: "length over limit", body: `{"safety_identifier":"` + strings.Repeat("x", 513) + `"}`, wantErr: ErrSafetyIdentifierLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(ValidatorContext{Document: parseDocument(t, tt.body)})
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestSafetyIdentifierValidatorRespectsCustomLimit(t *testing.T) {
	v := SafetyIdentifierValidator{MaxLen: 8}
	require.NoError(t, v.Validate(ValidatorContext{Document: parseDocument(t, `{"safety_identifier":"abcdefgh"}`)}))
	err := v.Validate(ValidatorContext{Document: parseDocument(t, `{"safety_identifier":"abcdefghi"}`)})
	require.ErrorIs(t, err, ErrSafetyIdentifierLength)
}
