package paramvalidators

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserValidatorAccepts(t *testing.T) {
	v := UserValidator{}
	tests := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{"messages":[]}`},
		{name: "empty string", body: `{"user":""}`},
		{name: "typical openai shape", body: `{"user":"user_abc123"}`},
		{name: "uuid", body: `{"user":"550e8400-e29b-41d4-a716-446655440000"}`},
		{name: "email shape", body: `{"user":"user@example.com"}`},
		{name: "base64-ish", body: `{"user":"YWJjZA+/=="}`},
		{name: "session id with colons", body: `{"user":"langchain:session:42"}`},
		{name: "exactly at length", body: `{"user":"` + strings.Repeat("x", 512) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, v.Validate(ValidatorContext{Document: parseDocument(t, tt.body)}))
		})
	}
}

func TestUserValidatorRejects(t *testing.T) {
	v := UserValidator{}
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "number", body: `{"user":42}`, wantErr: ErrUserShape},
		{name: "boolean", body: `{"user":true}`, wantErr: ErrUserShape},
		{name: "object", body: `{"user":{}}`, wantErr: ErrUserShape},
		{name: "array", body: `{"user":[]}`, wantErr: ErrUserShape},
		{name: "null", body: `{"user":null}`, wantErr: ErrUserShape},
		{name: "length over limit", body: `{"user":"` + strings.Repeat("x", 513) + `"}`, wantErr: ErrUserLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(ValidatorContext{Document: parseDocument(t, tt.body)})
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestUserValidatorRespectsCustomLimit(t *testing.T) {
	v := UserValidator{MaxLen: 8}
	require.NoError(t, v.Validate(ValidatorContext{Document: parseDocument(t, `{"user":"abcdefgh"}`)}))
	err := v.Validate(ValidatorContext{Document: parseDocument(t, `{"user":"abcdefghi"}`)})
	require.ErrorIs(t, err, ErrUserLength)
}
