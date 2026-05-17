package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeChatRequestDefaultsAndCapsOutputTokens(t *testing.T) {
	oldDefault := DefaultRequestMaxTokens
	oldCap := RequestMaxTokensCap
	DefaultRequestMaxTokens = 3_072
	RequestMaxTokensCap = 4_096
	t.Cleanup(func() {
		DefaultRequestMaxTokens = oldDefault
		RequestMaxTokensCap = oldCap
	})

	body, req, err := normalizeChatRequest([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 3_072, req.MaxTokens)
	require.Zero(t, req.MaxCompletionTokens)
	require.Contains(t, string(body), `"max_tokens":3072`)
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
	require.EqualValues(t, 4_096, req.MaxTokens)
	require.EqualValues(t, 4_096, req.MaxCompletionTokens)
	require.Contains(t, string(body), `"max_tokens":4096`)
	require.Contains(t, string(body), `"max_completion_tokens":4096`)

	body, req, err = normalizeChatRequest([]byte(`{"max_tokens":64,"max_completion_tokens":10000,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 64, req.MaxTokens)
	require.EqualValues(t, 64, req.MaxCompletionTokens)
	require.Contains(t, string(body), `"max_tokens":64`)
	require.Contains(t, string(body), `"max_completion_tokens":64`)
}

func TestNormalizeChatRequestUsesProvidedOutputTokenLimits(t *testing.T) {
	limits := outputTokenLimits{DefaultMaxTokens: 2_048, MaxTokensCap: 3_584}

	body, req, err := normalizeChatRequestForAuthAndLimits([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), false, limits)
	require.NoError(t, err)
	require.EqualValues(t, 2_048, req.MaxTokens)
	require.Contains(t, string(body), `"max_tokens":2048`)

	body, req, err = normalizeChatRequestForAuthAndLimits([]byte(`{"max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}`), false, limits)
	require.NoError(t, err)
	require.EqualValues(t, 3_584, req.MaxTokens)
	require.Contains(t, string(body), `"max_tokens":3584`)
}

func TestPrepareChatRequestBodyAdminAuthBypassesOutputTokenCap(t *testing.T) {
	oldDefault := DefaultRequestMaxTokens
	oldCap := RequestMaxTokensCap
	DefaultRequestMaxTokens = 3_072
	RequestMaxTokensCap = 4_096
	t.Cleanup(func() {
		DefaultRequestMaxTokens = oldDefault
		RequestMaxTokensCap = oldCap
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"max_tokens": 20000,
		"max_completion_tokens": 30000,
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	req = req.WithContext(context.WithValue(req.Context(), adminAuthContextKey{}, true))

	body, chatReq, err := prepareChatRequestBody(req)
	require.NoError(t, err)
	require.EqualValues(t, 20_000, chatReq.MaxTokens)
	require.EqualValues(t, 20_000, chatReq.MaxCompletionTokens)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 20_000, raw["max_tokens"])
	require.EqualValues(t, 20_000, raw["max_completion_tokens"])
}

func TestPrepareChatRequestBodyAdminAuthKeepsMaxCompletionTokensAboveDefault(t *testing.T) {
	oldDefault := DefaultRequestMaxTokens
	oldCap := RequestMaxTokensCap
	DefaultRequestMaxTokens = 3_072
	RequestMaxTokensCap = 4_096
	t.Cleanup(func() {
		DefaultRequestMaxTokens = oldDefault
		RequestMaxTokensCap = oldCap
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"max_completion_tokens": 30000,
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	req = req.WithContext(context.WithValue(req.Context(), adminAuthContextKey{}, true))

	body, chatReq, err := prepareChatRequestBody(req)
	require.NoError(t, err)
	require.EqualValues(t, 30_000, chatReq.MaxTokens)
	require.EqualValues(t, 30_000, chatReq.MaxCompletionTokens)
	require.NotContains(t, string(body), `"max_tokens"`)
	require.Contains(t, string(body), `"max_completion_tokens":30000`)
}

func TestPrepareChatRequestBodyPreservesLargeIntegerFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"seed": 9007199254740993,
		"messages": [{"role": "user", "content": "hello"}]
	}`))

	body, _, err := prepareChatRequestBody(req)
	require.NoError(t, err)

	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&raw))
	seed, ok := raw["seed"].(json.Number)
	require.True(t, ok)
	require.Equal(t, "9007199254740993", seed.String())
	require.Contains(t, string(body), `"seed":9007199254740993`)
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

func TestNormalizeChatRequestClampsMinTokensAboveEffectiveMax(t *testing.T) {
	oldDefault := DefaultRequestMaxTokens
	oldCap := RequestMaxTokensCap
	DefaultRequestMaxTokens = 1_000
	RequestMaxTokensCap = 2_000
	t.Cleanup(func() {
		DefaultRequestMaxTokens = oldDefault
		RequestMaxTokensCap = oldCap
	})

	body, req, err := normalizeChatRequest([]byte(`{
		"max_tokens": 9999,
		"min_tokens": 9994,
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)
	require.EqualValues(t, 2_000, req.MaxTokens)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 2_000, raw["max_tokens"])
	require.EqualValues(t, 2_000, raw["min_tokens"])
}

func TestNormalizeChatRequestKeepsMinTokensWithinEffectiveMax(t *testing.T) {
	oldDefault := DefaultRequestMaxTokens
	oldCap := RequestMaxTokensCap
	DefaultRequestMaxTokens = 1_000
	RequestMaxTokensCap = 2_000
	t.Cleanup(func() {
		DefaultRequestMaxTokens = oldDefault
		RequestMaxTokensCap = oldCap
	})

	body, req, err := normalizeChatRequest([]byte(`{
		"max_tokens": 9999,
		"min_tokens": 128,
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)
	require.EqualValues(t, 2_000, req.MaxTokens)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 2_000, raw["max_tokens"])
	require.EqualValues(t, 128, raw["min_tokens"])
}

func TestNormalizeChatRequestStripsTemperatureAboveMax(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"temperature": 999999
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 2.0, raw["temperature"])
}

func TestNormalizeChatRequestKeepsTemperatureWithinMax(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"temperature": 1.5
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 1.5, raw["temperature"])
}

func TestNormalizeChatRequestForcesValidationLogprobs(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"logprobs": false,
		"top_logprobs": 20
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Equal(t, true, raw["logprobs"])
	require.EqualValues(t, 5, raw["top_logprobs"])
}

func TestNormalizeChatRequestForcesLogprobsTrue(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"logprobs": false
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Equal(t, true, raw["logprobs"])
}

func TestNormalizeChatRequestForcesTopLogprobsFive(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"top_logprobs": 1
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 5, raw["top_logprobs"])
}

func TestNormalizeChatRequestRejectsPromptLogprobs(t *testing.T) {
	_, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"prompt_logprobs": 20
	}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "prompt_logprobs")
}

func TestNormalizeChatRequestStripsMinTokensWhenStopTokenIdsPresent(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"stop_token_ids": [163586, 9999999],
		"min_tokens": 1
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	_, exists := raw["min_tokens"]
	require.False(t, exists)
}

func TestNormalizeChatRequestConditionalMinTokensRuleTrueBranch(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"stop_token_ids": [7],
		"min_tokens": 3
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "min_tokens")
}

func TestNormalizeChatRequestKeepsMinTokensWithoutStopTokenIds(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"min_tokens": 5
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 5, raw["min_tokens"])
}

func TestNormalizeChatRequestConditionalMinTokensRuleFalseBranch(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"min_tokens": 3
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 3, raw["min_tokens"])
}

func TestNormalizeChatRequestStripsEmptyTools(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"tool_choice": "auto",
		"tools": [],
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "tools")
	require.NotContains(t, raw, "tool_choice")
}

func TestNormalizeChatRequestKeepsToolChoiceAutoWithTools(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"tool_choice": "auto",
		"tools": [{"type": "function", "function": {"name": "x", "description": "x", "parameters": {"type": "object"}}}],
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Equal(t, "auto", raw["tool_choice"])
	require.Contains(t, raw, "tools")
}

func TestNormalizeChatRequestRejectsUnsupportedVLLMParameters(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "beam search", body: `{"use_beam_search":true,"messages":[{"role":"user","content":"hello"}]}`, want: "use_beam_search"},
		{name: "truncate prompt tokens", body: `{"truncate_prompt_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, want: "truncate_prompt_tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := normalizeChatRequest([]byte(tt.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNormalizeChatRequestRejectsContractViolatingVLLMParameters(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "allowed token ids", body: `{"allowed_token_ids":[1,2,3],"messages":[{"role":"user","content":"hello"}]}`, want: "allowed_token_ids"},
		{name: "ignore eos", body: `{"ignore_eos":true,"messages":[{"role":"user","content":"hello"}]}`, want: "ignore_eos"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := normalizeChatRequest([]byte(tt.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNormalizeChatRequestStripsEmptyBadWords(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"bad_words": ["", "   ", "keep"],
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Equal(t, []any{"keep"}, raw["bad_words"])

	body, _, err = normalizeChatRequest([]byte(`{
		"bad_words": ["", "\t", "\n"],
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)
	raw = map[string]any{}
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "bad_words")
}

func TestNormalizeChatRequestStripsWhitespaceOnlyBadWordsResearchCases(t *testing.T) {
	tests := []struct {
		name     string
		badWords string
		want     []any
	}{
		{name: "empty string", badWords: `[""]`},
		{name: "empty then keep", badWords: `["", "foo"]`, want: []any{"foo"}},
		{name: "keep then empty", badWords: `["foo", ""]`, want: []any{"foo"}},
		{name: "ascii space", badWords: `[" "]`},
		{name: "multiple empties", badWords: `["", "", ""]`},
		{name: "tab", badWords: `["\t"]`},
		{name: "line feed", badWords: `["\n"]`},
		{name: "non breaking space", badWords: `["\u00A0"]`},
		{name: "cjk space", badWords: `["\u3000"]`},
		{name: "carriage return", badWords: `["\r"]`},
		{name: "vertical tab", badWords: `["\u000B"]`},
		{name: "form feed", badWords: `["\u000C"]`},
		{name: "multi space", badWords: `["  "]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _, err := normalizeChatRequest([]byte(`{
				"bad_words": ` + tt.badWords + `,
				"messages": [{"role": "user", "content": "hello"}]
			}`))
			require.NoError(t, err)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			if tt.want == nil {
				require.NotContains(t, raw, "bad_words")
				return
			}
			require.Equal(t, tt.want, raw["bad_words"])
		})
	}
}

func TestNormalizeChatRequestKeepsSafeBadWordsResearchCases(t *testing.T) {
	tests := []struct {
		name     string
		badWords string
		want     []any
	}{
		{name: "simple token", badWords: `["foo"]`, want: []any{"foo"}},
		{name: "empty list", badWords: `[]`},
		{name: "single character", badWords: `["a"]`, want: []any{"a"}},
		{name: "two words", badWords: `["foo", "bar"]`, want: []any{"foo", "bar"}},
		{name: "zero width space", badWords: `["\u200B"]`, want: []any{"\u200B"}},
		{name: "nul", badWords: `["\u0000"]`, want: []any{"\u0000"}},
		{name: "bom", badWords: `["\uFEFF"]`, want: []any{"\uFEFF"}},
		{name: "zero width joiner", badWords: `["\u200D"]`, want: []any{"\u200D"}},
		{name: "zero width non joiner", badWords: `["\u200C"]`, want: []any{"\u200C"}},
		{name: "combining mark", badWords: `["\u0301"]`, want: []any{"\u0301"}},
		{name: "variation selector", badWords: `["\uFE0F"]`, want: []any{"\uFE0F"}},
		{name: "left padded", badWords: `[" a"]`, want: []any{" a"}},
		{name: "right padded", badWords: `["a "]`, want: []any{"a "}},
		{name: "emoji", badWords: `["😀"]`, want: []any{"😀"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _, err := normalizeChatRequest([]byte(`{
				"bad_words": ` + tt.badWords + `,
				"messages": [{"role": "user", "content": "hello"}]
			}`))
			require.NoError(t, err)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			if tt.want == nil {
				require.NotContains(t, raw, "bad_words")
				return
			}
			require.Equal(t, tt.want, raw["bad_words"])
		})
	}
}

func TestNormalizeChatRequestStripsNonFiniteSamplingValues(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"temperature": "nan",
		"top_p": "inf",
		"min_p": "-inf",
		"repetition_penalty": "infinity",
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "temperature")
	require.NotContains(t, raw, "top_p")
	require.NotContains(t, raw, "min_p")
	require.NotContains(t, raw, "repetition_penalty")
}

func TestNormalizeChatRequestParsesStringEncodedSamplingValues(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"temperature": "1.2",
		"top_p": "0.5",
		"top_k": "40",
		"min_p": "0.1",
		"repetition_penalty": "1.2",
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.EqualValues(t, 1.2, raw["temperature"])
	require.EqualValues(t, 0.5, raw["top_p"])
	require.EqualValues(t, 40, raw["top_k"])
	require.EqualValues(t, 0.1, raw["min_p"])
	require.EqualValues(t, 1.2, raw["repetition_penalty"])
}

func TestNormalizeChatRequestStripsInvalidStringEncodedSamplingValues(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"temperature": "wat",
		"top_p": "",
		"top_k": "1.2.3",
		"min_p": "--1",
		"repetition_penalty": "not-a-number",
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "temperature")
	require.NotContains(t, raw, "top_p")
	require.NotContains(t, raw, "top_k")
	require.NotContains(t, raw, "min_p")
	require.NotContains(t, raw, "repetition_penalty")
}

func TestNormalizeChatRequestSanitizesRepetitionPenalty(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"repetition_penalty": 5,
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Equal(t, MaxRepetitionPenalty, raw["repetition_penalty"])

	body, _, err = normalizeChatRequest([]byte(`{
		"repetition_penalty": "5.0",
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)
	raw = map[string]any{}
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Equal(t, MaxRepetitionPenalty, raw["repetition_penalty"])
}

func TestNormalizeChatRequestStripsOutOfRangeLogitBias(t *testing.T) {
	body, _, err := normalizeChatRequest([]byte(`{
		"logit_bias": {"0": 1e30, "1": 10, "2": -101},
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Equal(t, map[string]any{"1": float64(10)}, raw["logit_bias"])

	body, _, err = normalizeChatRequest([]byte(`{
		"logit_bias": {"0": 1e30},
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	require.NoError(t, err)
	raw = map[string]any{}
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "logit_bias")
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
			wantStream:     false,
			wantToolChoice: nil,
		},
		{
			name:           "missing stream",
			body:           `{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: false},
			model:          kimiK26ModelID,
			wantStream:     false,
			wantToolChoice: nil,
		},
		{
			name:           "default kimi model",
			body:           `{"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Stream: false},
			model:          kimiK26ModelID,
			wantStream:     false,
			wantToolChoice: nil,
		},
		{
			name:           "already streaming",
			body:           `{"model":"moonshotai/Kimi-K2.6","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: true},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: nil,
		},
		{
			name:           "tool auto preserved",
			body:           `{"model":"moonshotai/Kimi-K2.6","stream":true,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"x","description":"x","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: true},
			model:          kimiK26ModelID,
			wantStream:     true,
			wantToolChoice: "auto",
		},
		{
			name:           "structured outputs unchanged",
			body:           `{"model":"moonshotai/Kimi-K2.6","stream":false,"structured_outputs":{"schema":{"type":"object"}},"messages":[{"role":"user","content":"hello"}]}`,
			req:            chatRequest{Model: kimiK26ModelID, Stream: false},
			model:          kimiK26ModelID,
			wantStream:     false,
			wantToolChoice: nil,
			wantStructured: true,
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
			if _, hasStream := raw["stream"]; hasStream {
				require.Equal(t, tt.wantStream, raw["stream"])
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

func TestNormalizeContentRejectsInvalidJSON(t *testing.T) {
	_, err := normalizeContent([]byte(`{"messages":[`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse request")
}

func TestApplyKimiRequestOverridesTranslatesMoonshotThinkingForVLLM(t *testing.T) {
	boolPtr := func(v bool) *bool {
		return &v
	}

	tests := []struct {
		name         string
		body         string
		model        string
		wantThinking *bool
		wantExtra    any
	}{
		{
			name:         "disabled",
			body:         `{"model":"moonshotai/Kimi-K2.6","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hello"}]}`,
			model:        kimiK26ModelID,
			wantThinking: boolPtr(false),
		},
		{
			name:         "enabled",
			body:         `{"model":"moonshotai/Kimi-K2.6","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hello"}]}`,
			model:        kimiK26ModelID,
			wantThinking: boolPtr(true),
		},
		{
			name:         "preserves other chat template kwargs",
			body:         `{"model":"moonshotai/Kimi-K2.6","thinking":{"type":"disabled"},"chat_template_kwargs":{"foo":"bar"},"messages":[{"role":"user","content":"hello"}]}`,
			model:        kimiK26ModelID,
			wantThinking: boolPtr(false),
			wantExtra:    "bar",
		},
		{
			name:         "explicit vllm thinking wins",
			body:         `{"model":"moonshotai/Kimi-K2.6","thinking":{"type":"enabled"},"chat_template_kwargs":{"thinking":false},"messages":[{"role":"user","content":"hello"}]}`,
			model:        kimiK26ModelID,
			wantThinking: boolPtr(false),
		},
		{
			name:  "invalid moonshot type is ignored",
			body:  `{"model":"moonshotai/Kimi-K2.6","thinking":{"type":"brief"},"messages":[{"role":"user","content":"hello"}]}`,
			model: kimiK26ModelID,
		},
		{
			name:  "non kimi unchanged",
			body:  `{"model":"Qwen/Test","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hello"}]}`,
			model: "Qwen/Test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _, err := applyKimiRequestOverrides([]byte(tt.body), chatRequest{Model: tt.model}, tt.model)
			require.NoError(t, err)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			kwargs, hasKwargs := raw["chat_template_kwargs"].(map[string]any)
			if tt.wantThinking == nil {
				require.False(t, hasKwargs)
				return
			}
			require.True(t, hasKwargs)
			require.Equal(t, *tt.wantThinking, kwargs["thinking"])
			if tt.wantExtra != nil {
				require.Equal(t, tt.wantExtra, kwargs["foo"])
			}
		})
	}
}

func TestNormalizeChatRequestRejectsUnsupportedPenaltyFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "presence penalty", body: `{"presence_penalty":1.2,"messages":[{"role":"user","content":"hello"}]}`, want: "presence_penalty"},
		{name: "frequency penalty", body: `{"frequency_penalty":0.8,"messages":[{"role":"user","content":"hello"}]}`, want: "frequency_penalty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := normalizeChatRequest([]byte(tt.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNormalizeChatRequestRejectsUnsupportedFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown top level field",
			body: `{"custom_param":true,"messages":[{"role":"user","content":"hello"}]}`,
			want: "custom_param",
		},
		{
			name: "enforced tokens",
			body: `{"enforced_tokens":["x"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "enforced_tokens",
		},
		{
			name: "json object response format",
			body: `{"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hello"}]}`,
			want: "response_format",
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
		{
			name: "json schema response format",
			body: `{"response_format":{"type":"json_schema"},"messages":[{"role":"user","content":"hello"}]}`,
			want: "response_format",
		},
		{
			name: "nested json schema response format",
			body: `{"response_format":{"json_schema":{"name":"r","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hello"}]}`,
			want: "response_format",
		},
		{
			name: "guided json",
			body: `{"guided_json":{"type":"object"},"messages":[{"role":"user","content":"hello"}]}`,
			want: "guided_json",
		},
		{
			name: "guided choice",
			body: `{"guided_choice":["a","b"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "guided_choice",
		},
		{
			name: "tool choice required",
			body: `{"tool_choice":"required","tools":[{"type":"function","function":{"name":"x","description":"x","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hello"}]}`,
			want: "tool_choice=required",
		},
		{
			name: "structured outputs",
			body: `{"structured_outputs":{"regex":"[a-z]+"},"messages":[{"role":"user","content":"hello"}]}`,
			want: "structured_outputs",
		},
		{
			name: "presence penalty",
			body: `{"presence_penalty":1.2,"messages":[{"role":"user","content":"hello"}]}`,
			want: "presence_penalty",
		},
		{
			name: "frequency penalty",
			body: `{"frequency_penalty":0.8,"messages":[{"role":"user","content":"hello"}]}`,
			want: "frequency_penalty",
		},
		{
			name: "prompt logprobs",
			body: `{"prompt_logprobs":20,"messages":[{"role":"user","content":"hello"}]}`,
			want: "prompt_logprobs",
		},
		{
			name: "beam search",
			body: `{"use_beam_search":true,"messages":[{"role":"user","content":"hello"}]}`,
			want: "use_beam_search",
		},
		{
			name: "truncate prompt tokens",
			body: `{"truncate_prompt_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
			want: "truncate_prompt_tokens",
		},
		{
			name: "allowed token ids",
			body: `{"allowed_token_ids":[1,2,3],"messages":[{"role":"user","content":"hello"}]}`,
			want: "allowed_token_ids",
		},
		{
			name: "ignore eos",
			body: `{"ignore_eos":true,"messages":[{"role":"user","content":"hello"}]}`,
			want: "ignore_eos",
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
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
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

func TestPrepareChatRequestBodyNormalizesTextContentParts(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "hello"},
				{"type": "text", "text": "world"}
			]
		}]
	}`))

	body, _, err := prepareChatRequestBody(req)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	messages := raw["messages"].([]any)
	message := messages[0].(map[string]any)
	require.Equal(t, "hello\nworld", message["content"])
}

func TestPrepareChatRequestBodyNormalizesEmptyAssistantToolCallContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: `""`},
		{name: "whitespace", content: `" "`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
				"messages": [
					{"role": "user", "content": "What is 2+2?"},
					{"role": "assistant", "content": `+tt.content+`, "tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "web_search", "arguments": "{\"query\":\"2+2\"}"}
					}]},
					{"role": "tool", "content": "4", "tool_call_id": "call_1"}
				]
			}`))

			body, _, err := prepareChatRequestBody(req)
			require.NoError(t, err)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			messages := raw["messages"].([]any)
			assistant := messages[1].(map[string]any)
			require.Contains(t, assistant, "content")
			require.Nil(t, assistant["content"])
		})
	}
}

func TestPrepareChatRequestBodyNormalizesEmptyToolContent(t *testing.T) {
	tests := []struct {
		name         string
		contentField string
	}{
		{name: "empty", contentField: `"content": "",`},
		{name: "whitespace", contentField: `"content": " ",`},
		{name: "null", contentField: `"content": null,`},
		{name: "missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
				"messages": [
					{"role": "user", "content": "What is 2+2?"},
					{"role": "assistant", "content": null, "tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "web_search", "arguments": "{\"query\":\"2+2\"}"}
					}]},
					{"role": "tool", `+tt.contentField+` "tool_call_id": "call_1"}
				]
			}`))

			body, _, err := prepareChatRequestBody(req)
			require.NoError(t, err)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			messages := raw["messages"].([]any)
			tool := messages[2].(map[string]any)
			require.Equal(t, emptyToolResultContent, tool["content"])
		})
	}
}

func TestPrepareChatRequestBodyRejectsNonTextContentParts(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "image_url",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]}`,
			want: `unsupported value "image_url"`,
		},
		{
			name: "input_text",
			body: `{"messages":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			want: `unsupported value "input_text"`,
		},
		{
			name: "unknown",
			body: `{"messages":[{"role":"user","content":[{"type":"custom","text":"hello"}]}]}`,
			want: `unsupported value "custom"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			_, _, err := prepareChatRequestBody(req)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
			require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
		})
	}
}

func TestPrepareChatRequestBodyAllowsExtraFieldsOnTextContentParts(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"messages": [{
			"role": "user",
			"content": [{
				"type": "text",
				"text": "hello",
				"cache_control": {"type": "ephemeral"}
			}]
		}]
	}`))

	body, _, err := prepareChatRequestBody(req)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	messages := raw["messages"].([]any)
	message := messages[0].(map[string]any)
	require.Equal(t, "hello", message["content"])
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
