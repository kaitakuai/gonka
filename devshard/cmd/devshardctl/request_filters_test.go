package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"devshard/cmd/devshardctl/paramvalidators"

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

	body, req, err := normalizeChatRequestForAuthAndLimits([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), false, limits, "")
	require.NoError(t, err)
	require.EqualValues(t, 2_048, req.MaxTokens)
	require.Contains(t, string(body), `"max_tokens":2048`)

	body, req, err = normalizeChatRequestForAuthAndLimits([]byte(`{"max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}`), false, limits, "")
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

func TestNormalizeChatRequestForcesSingleChoiceWithGreedySampling(t *testing.T) {
	// vLLM rejects `n > 1` when `temperature == 0` (greedy sampling produces identical
	// completions). Coerce silently to n=1 so 3000+ wasted retries don't reach the engine.
	coerceCases := []string{
		`{"n":2,"temperature":0,"messages":[{"role":"user","content":"hi"}]}`,
		`{"n":5,"temperature":0,"messages":[{"role":"user","content":"hi"}]}`,
		`{"n":5,"temperature":0.0,"messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, body := range coerceCases {
		t.Run("coerce_"+body, func(t *testing.T) {
			out, req, err := normalizeChatRequest([]byte(body))
			require.NoError(t, err)
			require.EqualValues(t, 1, req.N)
			require.Contains(t, string(out), `"n":1`)
		})
	}

	passThroughCases := []struct {
		body    string
		wantN   uint64
		wantStr string
	}{
		{body: `{"n":1,"temperature":0,"messages":[{"role":"user","content":"hi"}]}`, wantN: 1, wantStr: `"n":1`},
		{body: `{"n":5,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`, wantN: 5, wantStr: `"n":5`},
		{body: `{"n":5,"messages":[{"role":"user","content":"hi"}]}`, wantN: 5, wantStr: `"n":5`},
		{body: `{"n":5,"temperature":0.0001,"messages":[{"role":"user","content":"hi"}]}`, wantN: 5, wantStr: `"n":5`},
	}
	for _, tc := range passThroughCases {
		t.Run("keep_"+tc.body, func(t *testing.T) {
			out, req, err := normalizeChatRequest([]byte(tc.body))
			require.NoError(t, err)
			require.EqualValues(t, tc.wantN, req.N)
			require.Contains(t, string(out), tc.wantStr)
		})
	}
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

func TestNormalizeChatRequestClampsFrequencyAndPresencePenalty(t *testing.T) {
	// OpenAI/vLLM accept [-2.0, 2.0] for both. Catalog clamps; out-of-range is rewritten,
	// not rejected. Per-Kimi force-zero is exercised separately under ApplyKimiRequestOverrides.
	tests := []struct {
		name  string
		body  string
		field string
		want  float64
	}{
		{name: "freq above max", body: `{"messages":[{"role":"user","content":"hi"}],"frequency_penalty":5}`, field: "frequency_penalty", want: 2.0},
		{name: "freq below min", body: `{"messages":[{"role":"user","content":"hi"}],"frequency_penalty":-5}`, field: "frequency_penalty", want: -2.0},
		{name: "freq within range", body: `{"messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.5}`, field: "frequency_penalty", want: 0.5},
		{name: "pres above max", body: `{"messages":[{"role":"user","content":"hi"}],"presence_penalty":3.5}`, field: "presence_penalty", want: 2.0},
		{name: "pres below min", body: `{"messages":[{"role":"user","content":"hi"}],"presence_penalty":-3.5}`, field: "presence_penalty", want: -2.0},
		{name: "pres zero", body: `{"messages":[{"role":"user","content":"hi"}],"presence_penalty":0}`, field: "presence_penalty", want: 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _, err := normalizeChatRequest([]byte(tt.body))
			require.NoError(t, err)
			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			require.EqualValues(t, tt.want, raw[tt.field])
		})
	}
}

func TestNormalizeChatRequestStripsNonFiniteFrequencyAndPresencePenalty(t *testing.T) {
	tests := []string{
		`{"messages":[{"role":"user","content":"hi"}],"frequency_penalty":"infinity"}`,
		`{"messages":[{"role":"user","content":"hi"}],"frequency_penalty":"nan"}`,
		`{"messages":[{"role":"user","content":"hi"}],"frequency_penalty":"not-a-number"}`,
		`{"messages":[{"role":"user","content":"hi"}],"presence_penalty":"infinity"}`,
	}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			out, _, err := normalizeChatRequest([]byte(body))
			require.NoError(t, err)
			var raw map[string]any
			require.NoError(t, json.Unmarshal(out, &raw))
			require.NotContains(t, raw, "frequency_penalty")
			require.NotContains(t, raw, "presence_penalty")
		})
	}
}

func TestNormalizeForKimiForceZerosPenalties(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "freq nonzero", body: `{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.5}`},
		{name: "pres nonzero", body: `{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hi"}],"presence_penalty":-1.5}`},
		{name: "both nonzero", body: `{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hi"}],"frequency_penalty":2,"presence_penalty":-2}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _, err := normalizeChatRequestForModel([]byte(tt.body), kimiK26ModelID)
			require.NoError(t, err)
			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw))
			if _, has := raw["frequency_penalty"]; has {
				require.EqualValues(t, 0.0, raw["frequency_penalty"])
			}
			if _, has := raw["presence_penalty"]; has {
				require.EqualValues(t, 0.0, raw["presence_penalty"])
			}
		})
	}
}

func TestNormalizeForKimiLeavesPenaltiesAlreadyZero(t *testing.T) {
	body := `{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0,"presence_penalty":0}`
	out, _, err := normalizeChatRequestForModel([]byte(body), kimiK26ModelID)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.EqualValues(t, 0.0, raw["frequency_penalty"])
	require.EqualValues(t, 0.0, raw["presence_penalty"])
}

func TestNormalizeDoesNotForceZeroPenaltiesForOtherModels(t *testing.T) {
	body := `{"model":"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.5,"presence_penalty":-0.5}`
	out, _, err := normalizeChatRequestForModel([]byte(body), "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8")
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.EqualValues(t, 0.5, raw["frequency_penalty"])
	require.EqualValues(t, -0.5, raw["presence_penalty"])
}

func TestNormalizeForKimiDoesNotAddPenaltiesWhenAbsent(t *testing.T) {
	body := `{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hi"}]}`
	out, _, err := normalizeChatRequestForModel([]byte(body), kimiK26ModelID)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.NotContains(t, raw, "frequency_penalty")
	require.NotContains(t, raw, "presence_penalty")
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

func TestNormalizeChatRequestDefaultsToolChoiceToAutoWhenToolsProvided(t *testing.T) {
	// When the client passes `tools` without `tool_choice`, the gateway substitutes the
	// OpenAI-spec default ("auto") so downstream vLLM never sees an absent value -- vLLM's
	// own default routes through code that requires --enable-auto-tool-choice, and 66
	// captured failures showed clients consistently dropping the field.
	body := `{
		"tools": [{"type": "function", "function": {"name": "x", "parameters": {"type": "object"}}}],
		"messages": [{"role": "user", "content": "hi"}]
	}`
	out, _, err := normalizeChatRequest([]byte(body))
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "auto", raw["tool_choice"])
}

func TestNormalizeChatRequestCoercesRequiredToAuto(t *testing.T) {
	body := `{
		"tool_choice": "required",
		"tools": [{"type":"function","function":{"name":"x","parameters":{"type":"object"}}}],
		"messages": [{"role":"user","content":"hi"}]
	}`
	out, _, err := normalizeChatRequest([]byte(body))
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "auto", raw["tool_choice"])
}

func TestNormalizeChatRequestRejectsMalformedToolChoice(t *testing.T) {
	tests := []string{
		`{"tool_choice":"force","messages":[{"role":"user","content":"hi"}]}`,
		`{"tool_choice":42,"messages":[{"role":"user","content":"hi"}]}`,
		`{"tool_choice":true,"messages":[{"role":"user","content":"hi"}]}`,
		`{"tool_choice":["auto"],"messages":[{"role":"user","content":"hi"}]}`,
		`{"tool_choice":{"type":"plugin","function":{"name":"x"}},"messages":[{"role":"user","content":"hi"}]}`,
		`{"tool_choice":{"type":"function"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"tool_choice":{"type":"function","function":{}},"messages":[{"role":"user","content":"hi"}]}`,
		`{"tool_choice":{"type":"function","function":{"name":""}},"messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			_, _, err := normalizeChatRequest([]byte(body))
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
			require.Contains(t, err.Error(), "tool_choice")
		})
	}
}

func TestNormalizeChatRequestKeepsExplicitToolChoiceValues(t *testing.T) {
	choices := []string{`"auto"`, `"none"`}
	for _, tc := range choices {
		t.Run(tc, func(t *testing.T) {
			body := `{
				"tool_choice": ` + tc + `,
				"tools": [{"type": "function", "function": {"name": "x", "parameters": {"type": "object"}}}],
				"messages": [{"role": "user", "content": "hi"}]
			}`
			out, _, err := normalizeChatRequest([]byte(body))
			require.NoError(t, err)
			var raw map[string]any
			require.NoError(t, json.Unmarshal(out, &raw))
			require.Equal(t, strings.Trim(tc, `"`), raw["tool_choice"])
		})
	}

	t.Run("function object", func(t *testing.T) {
		body := `{
			"tool_choice": {"type":"function","function":{"name":"x"}},
			"tools": [{"type": "function", "function": {"name": "x", "parameters": {"type": "object"}}}],
			"messages": [{"role": "user", "content": "hi"}]
		}`
		out, _, err := normalizeChatRequest([]byte(body))
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(out, &raw))
		choice, ok := raw["tool_choice"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "function", choice["type"])
	})
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

func TestNormalizeContentRejectsInvalidJSON(t *testing.T) {
	_, err := normalizeContent([]byte(`{"messages":[`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse request")
}

func TestNormalizeForKimiMirrorsThinkingToTemplateKwargs(t *testing.T) {
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
			name:  "non kimi unchanged",
			body:  `{"model":"Qwen/Test","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hello"}]}`,
			model: "Qwen/Test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _, err := normalizeChatRequestForModel([]byte(tt.body), tt.model)
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
			// JSON accepts "" as a key; the whitelist rejects it with a dedicated message
			name: "empty string key",
			body: `{"":"slip","messages":[{"role":"user","content":"hello"}]}`,
			want: "field with an empty name",
		},
		{
			name: "enforced tokens",
			body: `{"enforced_tokens":["x"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "enforced_tokens",
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
			name: "structured outputs",
			body: `{"structured_outputs":{"regex":"[a-z]+"},"messages":[{"role":"user","content":"hello"}]}`,
			want: "structured_outputs",
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

// Integration coverage that response_format is routed through the catalog rule and that pipeline
// errors are translated into HTTP 400. Exhaustive validator behavior lives in
// filters_parameters/response_format_test.go.
func TestNormalizeChatRequestResponseFormatPipeline(t *testing.T) {
	t.Run("accepts type text", func(t *testing.T) {
		body, _, err := normalizeChatRequest([]byte(`{"response_format":{"type":"text"},"messages":[{"role":"user","content":"hello"}]}`))
		require.NoError(t, err)
		require.Contains(t, string(body), `"response_format"`)
	})

	t.Run("accepts json_schema with simple schema", func(t *testing.T) {
		body, _, err := normalizeChatRequest([]byte(`{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object","properties":{"x":{"type":"string"}}}}},"messages":[{"role":"user","content":"hello"}]}`))
		require.NoError(t, err)
		require.Contains(t, string(body), `"response_format"`)
	})

	t.Run("rejects unknown type with HTTP 400", func(t *testing.T) {
		_, _, err := normalizeChatRequest([]byte(`{"response_format":{"type":"banana"},"messages":[{"role":"user","content":"hello"}]}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "response_format")
		require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
	})

	t.Run("rejects pathological recursive schema with HTTP 400", func(t *testing.T) {
		deepSchema := `{"type":"object"}`
		for i := 0; i < 200; i++ {
			deepSchema = `{"type":"object","properties":{"x":` + deepSchema + `}}`
		}
		body := `{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` + deepSchema + `}},"messages":[{"role":"user","content":"hello"}]}`
		_, _, err := normalizeChatRequest([]byte(body))
		require.Error(t, err)
		require.Contains(t, err.Error(), "depth")
		require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
	})
}

func TestNormalizeChatRequestChatTemplateKwargsDepthBoundary(t *testing.T) {
	nestedChain := func(n int) string {
		s := `{}`
		for i := 1; i < n; i++ {
			s = `{"x":` + s + `}`
		}
		return s
	}

	t.Run("accepts chat_template_kwargs at depth limit", func(t *testing.T) {
		body := `{"chat_template_kwargs":` + nestedChain(16) + `,"messages":[{"role":"user","content":"hi"}]}`
		_, _, err := normalizeChatRequest([]byte(body))
		require.NoError(t, err)
	})

	t.Run("rejects chat_template_kwargs one level past limit with HTTP 400", func(t *testing.T) {
		body := `{"chat_template_kwargs":` + nestedChain(17) + `,"messages":[{"role":"user","content":"hi"}]}`
		_, _, err := normalizeChatRequest([]byte(body))
		require.Error(t, err)
		require.True(t, errors.Is(err, paramvalidators.ErrSchemaDepth),
			"expected ErrSchemaDepth (validator-layer reject), got: %v", err)
		require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
	})
}

func TestDefaultCatalogSchemaDepthLimits(t *testing.T) {
	const expected = 16

	findValidator := func(t *testing.T, name string) DocumentValidator {
		t.Helper()
		for _, p := range defaultParameterCatalog.parameters {
			if p.Name != name {
				continue
			}
			for _, rule := range p.Rules {
				h, ok := rule.Handler.(DocumentValidatorHandler)
				if !ok {
					continue
				}
				return h.Validator
			}
		}
		t.Fatalf("no DocumentValidator wired for parameter %q in defaultParameterCatalog", name)
		return nil
	}

	t.Run("response_format", func(t *testing.T) {
		v, ok := findValidator(t, "response_format").(paramvalidators.ResponseFormatValidator)
		require.True(t, ok, "response_format validator is not ResponseFormatValidator")
		require.Equal(t, expected, v.MaxDepth)
	})

	t.Run("tools", func(t *testing.T) {
		v, ok := findValidator(t, "tools").(paramvalidators.ToolsValidator)
		require.True(t, ok, "tools validator is not ToolsValidator")
		require.Equal(t, expected, v.MaxDepth)
	})

	t.Run("chat_template_kwargs", func(t *testing.T) {
		v, ok := findValidator(t, "chat_template_kwargs").(paramvalidators.ChatTemplateKwargsValidator)
		require.True(t, ok, "chat_template_kwargs validator is not ChatTemplateKwargsValidator")
		require.Equal(t, expected, v.MaxDepth)
	})
}

func TestNormalizeChatRequestEnforcesListCaps(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "stop too many entries",
			body: `{"stop":[` + strings.Repeat(`"a",`, 16) + `"b"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "stop",
		},
		{
			name: "stop entry too long",
			body: `{"stop":["` + strings.Repeat("a", 257) + `"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "stop[0]",
		},
		{
			name: "stop_token_ids too many entries",
			body: `{"stop_token_ids":[` + strings.Repeat(`1,`, 64) + `2],"messages":[{"role":"user","content":"hello"}]}`,
			want: "stop_token_ids",
		},
		{
			name: "bad_words too many entries",
			body: `{"bad_words":[` + strings.Repeat(`"a",`, 64) + `"b"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "bad_words",
		},
		{
			name: "bad_words entry too long",
			body: `{"bad_words":["` + strings.Repeat("a", 129) + `"],"messages":[{"role":"user","content":"hello"}]}`,
			want: "bad_words[0]",
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

func TestNormalizeChatRequestEnforcesMessagesCountCap(t *testing.T) {
	// Build a body with 2049 minimal valid user messages -- one over the cap.
	var b strings.Builder
	b.WriteString(`{"messages":[`)
	for i := 0; i < 2049; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"hi"}`)
	}
	b.WriteString(`]}`)

	_, _, err := normalizeChatRequest([]byte(b.String()))
	require.Error(t, err)
	require.Contains(t, err.Error(), "messages")
	require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
}

func TestNormalizeChatRequestAcceptsMessagesAtCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"messages":[`)
	for i := 0; i < 2048; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"hi"}`)
	}
	b.WriteString(`]}`)

	_, _, err := normalizeChatRequest([]byte(b.String()))
	require.NoError(t, err)
}

func TestNormalizeChatRequestValidatesSeed(t *testing.T) {
	t.Run("accepts non-negative integer", func(t *testing.T) {
		_, _, err := normalizeChatRequest([]byte(`{"seed":42,"messages":[{"role":"user","content":"hello"}]}`))
		require.NoError(t, err)
	})
	t.Run("accepts absent seed", func(t *testing.T) {
		_, _, err := normalizeChatRequest([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
		require.NoError(t, err)
	})
	t.Run("rejects negative seed", func(t *testing.T) {
		_, _, err := normalizeChatRequest([]byte(`{"seed":-5,"messages":[{"role":"user","content":"hello"}]}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "seed")
		require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
	})
	t.Run("rejects float seed", func(t *testing.T) {
		_, _, err := normalizeChatRequest([]byte(`{"seed":3.14,"messages":[{"role":"user","content":"hello"}]}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "seed")
	})
	t.Run("rejects string seed", func(t *testing.T) {
		_, _, err := normalizeChatRequest([]byte(`{"seed":"42","messages":[{"role":"user","content":"hello"}]}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "seed")
	})
}

func TestNormalizeChatRequestEnforcesLogitBiasMapCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"logit_bias":{`)
	for i := 0; i < 1025; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":1`)
	}
	b.WriteString(`},"messages":[{"role":"user","content":"hello"}]}`)

	_, _, err := normalizeChatRequest([]byte(b.String()))
	require.Error(t, err)
	require.Contains(t, err.Error(), "logit_bias")
	require.Equal(t, http.StatusBadRequest, chatRequestErrorStatus(err, http.StatusInternalServerError))
}

func TestNormalizeChatRequestAcceptsListCapsAtLimit(t *testing.T) {
	t.Run("stop at exact entry limit", func(t *testing.T) {
		body := `{"stop":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 16), ",") + `],"messages":[{"role":"user","content":"hello"}]}`
		_, _, err := normalizeChatRequest([]byte(body))
		require.NoError(t, err)
	})
	t.Run("stop_token_ids at exact entry limit", func(t *testing.T) {
		body := `{"stop_token_ids":[` + strings.TrimSuffix(strings.Repeat(`1,`, 64), ",") + `],"messages":[{"role":"user","content":"hello"}]}`
		_, _, err := normalizeChatRequest([]byte(body))
		require.NoError(t, err)
	})
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

func TestEnsureRequestNestingDepth(t *testing.T) {
	// nestedObjects produces `{"a":{"a":...}}` of the given object-nesting depth.
	nestedObjects := func(depth int) []byte {
		return []byte(strings.Repeat(`{"a":`, depth) + `1` + strings.Repeat(`}`, depth))
	}

	t.Run("at limit accepted", func(t *testing.T) {
		require.NoError(t, ensureRequestNestingDepth(nestedObjects(MaxRequestNestingDepth), MaxRequestNestingDepth))
	})

	t.Run("one over limit rejected", func(t *testing.T) {
		err := ensureRequestNestingDepth(nestedObjects(MaxRequestNestingDepth+1), MaxRequestNestingDepth)
		require.Error(t, err)
		require.Contains(t, err.Error(), "request nesting depth exceeds limit")
	})

	t.Run("array nesting counts equally", func(t *testing.T) {
		body := []byte(strings.Repeat(`[`, MaxRequestNestingDepth+1) + `1` + strings.Repeat(`]`, MaxRequestNestingDepth+1))
		err := ensureRequestNestingDepth(body, MaxRequestNestingDepth)
		require.Error(t, err)
	})

	t.Run("braces inside strings do not count", func(t *testing.T) {
		// Without string-awareness, this body would appear to nest 100 deep.
		body := []byte(`{"k":"` + strings.Repeat(`{`, 100) + `"}`)
		require.NoError(t, ensureRequestNestingDepth(body, MaxRequestNestingDepth))
	})

	t.Run("escaped quote inside string", func(t *testing.T) {
		// The escaped quote must not exit string mode; the trailing braces inside the
		// string still must not be counted.
		body := []byte(`{"k":"x\"` + strings.Repeat(`{`, 100) + `"}`)
		require.NoError(t, ensureRequestNestingDepth(body, MaxRequestNestingDepth))
	})

	t.Run("imbalanced closers rebase to zero", func(t *testing.T) {
		// Excess closers must not let a later valid block bypass the limit by going negative.
		body := []byte(strings.Repeat(`}`, 50) + strings.Repeat(`{`, MaxRequestNestingDepth+1))
		err := ensureRequestNestingDepth(body, MaxRequestNestingDepth)
		require.Error(t, err)
	})
}

func TestNormalizeChatRequestRejectsBodyAtNestingLimit(t *testing.T) {
	// Pipeline-level proof that the pre-scan participates in normalizeChatRequest.
	deep := `"x"`
	for i := 0; i < MaxRequestNestingDepth+1; i++ {
		deep = `{"a":` + deep + `}`
	}
	body := `{"messages":[{"role":"user","content":` + deep + `}]}`
	_, _, err := normalizeChatRequest([]byte(body))
	require.Error(t, err)
	require.Contains(t, err.Error(), "request nesting depth exceeds limit")
}

// Regression guard for the document mutex: without proper locking this trips
// Go's fatal "concurrent map writes" or the race detector.
func TestChatRequestDocumentConcurrentAccess(t *testing.T) {
	doc, err := decodeChatRequestDocument([]byte(`{"a":1,"b":2,"c":3}`))
	require.NoError(t, err)

	const workers = 32
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			key := "k" + strconv.Itoa(id)
			for i := 0; i < iterations; i++ {
				doc.Set(key, i)
				_, _ = doc.Get(key)
				_ = doc.Has("a")
				_, _ = doc.String("missing")
				switch i % 4 {
				case 0:
					_ = doc.Keys()
				case 1:
					_, _ = doc.Marshal()
				case 2:
					doc.RLockedScope(func(raw map[string]any) {
						for range raw {
						}
					})
				case 3:
					doc.LockedScope(func(raw map[string]any) {
						raw["shared"] = i
					})
				}
			}
			doc.Delete(key)
		}(w)
	}
	wg.Wait()
}

// End-to-end coverage that the four OpenAI Chat Completions observability fields survive
// the catalog's unknown-key gate, with `metadata` bounded and `stream_options` sanitized.
// Without these catalog entries the gate would 400 every legitimate OpenAI-built client
// (official SDK with `user=...`, LangChain with `metadata={...}`, any streaming client
// asking for final-chunk usage).
func TestNormalizeChatRequestAcceptsOpenAIObservabilityFields(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"user":"alice",
		"metadata":{"trace_id":"abc","span_id":"def"},
		"parallel_tool_calls":false,
		"stream_options":{"include_usage":true}
	}`)
	out, _, err := normalizeChatRequest(body)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "alice", raw["user"])
	require.Equal(t, map[string]any{"trace_id": "abc", "span_id": "def"}, raw["metadata"])
	require.Equal(t, false, raw["parallel_tool_calls"])
	so, ok := raw["stream_options"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, so["include_usage"])
}

// Pipeline-level coverage of StreamOptionsValidator: `continuous_usage_stats` drops out
// (vLLM-project/vllm#9028), `include_usage` survives.
func TestNormalizeChatRequestStripsContinuousUsageStats(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"stream_options":{"include_usage":true,"continuous_usage_stats":true}
	}`)
	out, _, err := normalizeChatRequest(body)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	so := raw["stream_options"].(map[string]any)
	require.Equal(t, true, so["include_usage"])
	require.NotContains(t, so, "continuous_usage_stats")
}

// Pipeline-level coverage: stream_options that empties out after sanitize is dropped.
func TestNormalizeChatRequestDropsEmptiedStreamOptions(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"stream_options":{"continuous_usage_stats":true}
	}`)
	out, _, err := normalizeChatRequest(body)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.NotContains(t, raw, "stream_options")
}

// Pipeline-level coverage of MetadataValidator: too many keys / oversize values are
// rejected.
func TestNormalizeChatRequestRejectsOversizedMetadata(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"metadata":{"k":"` + strings.Repeat("v", 513) + `"}
	}`)
	_, _, err := normalizeChatRequest(body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "metadata")
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetDefaultsForKimi(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":4096}`),
		kimiK26ModelID,
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":2048`)
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetRespectsClientValue(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":4096,"thinking_token_budget":500}`),
		kimiK26ModelID,
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":500`)
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetClampsAboveMaxTokens(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":4096,"thinking_token_budget":10000}`),
		kimiK26ModelID,
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":4096`)
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetClampsAboveAbsoluteMax(t *testing.T) {
	oldCap := RequestMaxTokensCap
	RequestMaxTokensCap = 200_000
	t.Cleanup(func() { RequestMaxTokensCap = oldCap })

	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":200000,"thinking_token_budget":150000}`),
		kimiK26ModelID,
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":96000`)
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetSmallMaxTokensSplitsInHalf(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":200}`),
		kimiK26ModelID,
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":100`)
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetHalfSplitMidRange(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":400}`),
		kimiK26ModelID,
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":200`)
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetEnforcedEvenWhenDisabled(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":4096,"thinking":{"type":"disabled"}}`),
		kimiK26ModelID,
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":2048`)
}

func TestNormalizeChatRequestKimiThinkingTokenBudgetNotInjectedForOtherModels(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":4096}`),
		"some/other-model",
	)
	require.NoError(t, err)
	require.NotContains(t, string(body), `thinking_token_budget`)
}

func TestNormalizeChatRequestThinkingTokenBudgetClampedForAllModels(t *testing.T) {
	body, _, err := normalizeChatRequestForModel(
		[]byte(`{"messages":[{"role":"user","content":"x"}],"max_tokens":4096,"thinking_token_budget":200000}`),
		"some/other-model",
	)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thinking_token_budget":4096`)
}
