package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeChatRequestDefaultsAndCapsOutputTokens(t *testing.T) {
	oldDefault := DefaultRequestMaxTokens
	DefaultRequestMaxTokens = 10_000
	t.Cleanup(func() {
		DefaultRequestMaxTokens = oldDefault
	})

	body, req, err := normalizeChatRequest([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 10_000, req.MaxTokens)
	require.Zero(t, req.MaxCompletionTokens)
	require.Contains(t, string(body), `"max_tokens":10000`)
	require.NotContains(t, string(body), `"max_completion_tokens"`)

	body, req, err = normalizeChatRequest([]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 64, req.MaxTokens)
	require.Zero(t, req.MaxCompletionTokens)
	require.Contains(t, string(body), `"max_tokens":64`)
	require.NotContains(t, string(body), `"max_completion_tokens"`)

	body, req, err = normalizeChatRequest([]byte(`{"max_completion_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 64, req.MaxTokens)
	require.EqualValues(t, 64, req.MaxCompletionTokens)
	require.NotContains(t, string(body), `"max_tokens"`)
	require.Contains(t, string(body), `"max_completion_tokens":64`)

	body, req, err = normalizeChatRequest([]byte(`{"max_tokens":10001,"max_completion_tokens":20000,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 10_000, req.MaxTokens)
	require.EqualValues(t, 10_000, req.MaxCompletionTokens)
	require.Contains(t, string(body), `"max_tokens":10000`)
	require.Contains(t, string(body), `"max_completion_tokens":10000`)

	body, req, err = normalizeChatRequest([]byte(`{"max_tokens":64,"max_completion_tokens":10000,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 64, req.MaxTokens)
	require.EqualValues(t, 64, req.MaxCompletionTokens)
	require.Contains(t, string(body), `"max_tokens":64`)
	require.Contains(t, string(body), `"max_completion_tokens":64`)
}

func TestNormalizeChatRequestCapsChoices(t *testing.T) {
	body, req, err := normalizeChatRequest([]byte(`{"n":1638400,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, MaxChatRequestChoices, req.N)
	require.Contains(t, string(body), `"n":5`)

	body, req, err = normalizeChatRequest([]byte(`{"n":3,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 3, req.N)
	require.Contains(t, string(body), `"n":3`)

	body, req, err = normalizeChatRequest([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.Zero(t, req.N)
	require.NotContains(t, string(body), `"n"`)
}

func TestApplyKimiRequestOverrides(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		req            chatRequest
		model          string
		wantStream     bool
		wantToolChoice any
		wantStructured bool
	}{
		{
			name:           "explicit false",
			body:           `{"model":"moonshotai/Kimi-K2.6","stream":false,"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: false},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: "none",
		},
		{
			name:           "missing stream",
			body:           `{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: false},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: "none",
		},
		{
			name:           "default kimi model",
			body:           `{"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Stream: false},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: "none",
		},
		{
			name:           "already streaming",
			body:           `{"model":"moonshotai/Kimi-K2.6","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: true},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: "none",
		},
		{
			name:           "tool auto becomes none",
			body:           `{"model":"moonshotai/Kimi-K2.6","stream":true,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"x","description":"x","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: true},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: "none",
		},
		{
			name:           "structured outputs removed",
			body:           `{"model":"moonshotai/Kimi-K2.6","stream":false,"structured_outputs":{"schema":{"type":"object"}},"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: false},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: "none",
		},
		{
			name:           "non kimi unchanged",
			body:           `{"model":"Qwen/Test","stream":false,"structured_outputs":{"schema":{"type":"object"}},"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: "Qwen/Test", Stream: false},
			model:          "Qwen/Test",
			wantStream:     false,
			wantToolChoice: nil,
			wantStructured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, req, err := applyKimiRequestOverrides([]byte(tt.body), tt.req, tt.model)
			require.NoError(t, err)
			require.Equal(t, tt.wantStream, req.Stream)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			if tt.wantStream {
				require.Equal(t, true, raw["stream"])
			} else {
				require.Equal(t, false, raw["stream"])
			}
			if tt.wantToolChoice == nil {
				require.NotContains(t, raw, "tool_choice")
			} else {
				require.Equal(t, tt.wantToolChoice, raw["tool_choice"])
			}
			if tt.wantStructured {
				require.Contains(t, raw, "structured_outputs")
			} else {
				require.NotContains(t, raw, "structured_outputs")
			}
		})
	}
}

func TestNormalizeChatRequestStripsStructuredOutputsBeforeValidation(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"model": "Qwen/Test",
		"structured_outputs": {"regex": "[a-z]+"},
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "structured_outputs")
}

func TestNormalizeChatRequestStripsUnsupportedPenaltyFields(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"presence_penalty": 1.2,
		"frequency_penalty": 0.8,
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "presence_penalty")
	require.NotContains(t, raw, "frequency_penalty")
}

func TestNormalizeChatRequestRejectsUnsupportedFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "enforced tokens",
			body: `{"enforced_tokens":["x"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "enforced_tokens",
		},
		{
			name: "json object response format",
			body: `{"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hello"}]}`,
			want: "json_object",
		},
		{
			name: "guided regex",
			body: `{"guided_regex":"[a-z]+","messages":[{"role":"user","content":"hello"}]}`,
			want: "guided_regex",
		},
		{
			name: "guided grammar",
			body: `{"guided_grammar":"root ::= item","messages":[{"role":"user","content":"hello"}]}`,
			want: "guided_grammar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := normalizeChatRequest([]byte(tt.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
			require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
		})
	}
}

func TestNormalizeChatRequestRejectsMalformedMessages(t *testing.T) {
	tests := []string{
		`{"messages":"hello"}`,
		`{"messages":[]}`,
		`{"messages":[{"content":"hello"}]}`,
		`{"messages":[{"role":"user","content":123}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		`{"messages":[{"role":"tool","tool_call_id":"missing","content":"hello"}]}`,
	}

	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			_, _, err := normalizeChatRequest([]byte(body))
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
		})
	}
}

func TestPrepareChatRequestBodyRejectsBodiesLargerThanTenMiB(t *testing.T) {
	tooLarge := bytes.Repeat([]byte("a"), MaxChatRequestBodySize+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(tooLarge))

	_, _, err := prepareChatRequestBody(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request body too large")
	require.Equal(t, http.StatusRequestEntityTooLarge, chatRequestErrorStatus(err, http.StatusBadRequest))
}

func TestPrepareChatRequestBodyAcceptsTenMiBBody(t *testing.T) {
	paddingSize := MaxChatRequestBodySize - len(`{"messages":[{"role":"user","content":""}]}`)
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", paddingSize) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	_, _, err := prepareChatRequestBody(req)
	require.NoError(t, err)
}
