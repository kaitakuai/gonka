package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const MaxChatRequestBodySize = 10 * 1024 * 1024
const MaxChatRequestChoices = 5
const MaxTemperature = 2.0
const MaxRepetitionPenalty = 2.0
const kimiK26ModelID = "moonshotai/Kimi-K2.6"
const emptyToolResultContent = "<empty tool result>"

const (
	kimiThinkingTokenBudgetDefaultDivisor uint64 = 2
	kimiThinkingTokenBudgetMin            uint64 = 256
	kimiThinkingTokenBudgetMax            uint64 = 96_000
)

type chatRequest struct {
	Model               string `json:"model"`
	Stream              bool   `json:"stream"`
	MaxTokens           uint64 `json:"max_tokens"`
	MaxCompletionTokens uint64 `json:"max_completion_tokens"`
	N                   uint64 `json:"n"`
}

type outputTokenLimits struct {
	DefaultMaxTokens uint64
	MaxTokensCap     uint64
}

type chatRequestFilterError struct {
	status  int
	message string
	wrapped error
}

func (e *chatRequestFilterError) Error() string {
	return e.message
}

// Unwrap exposes the underlying error so callers can use errors.Is/errors.As to identify
// the rejection class even though the outer error carries an HTTP status. This lets pure
// validators in subpackages (filters_parameters/...) define sentinel errors and have them
// remain detectable after passing through the badChatRequest wrapper.
func (e *chatRequestFilterError) Unwrap() error {
	return e.wrapped
}

func chatRequestErrorStatus(err error, fallback int) int {
	var filterErr *chatRequestFilterError
	if errors.As(err, &filterErr) {
		return filterErr.status
	}
	return fallback
}

func badChatRequest(format string, args ...any) error {
	return &chatRequestFilterError{
		status:  http.StatusBadRequest,
		message: fmt.Sprintf(format, args...),
	}
}

// wrapBadChatRequest preserves the error chain (so errors.Is/As works) while attaching the
// HTTP 400 status that the gateway uses for rejection.
func wrapBadChatRequest(err error) error {
	return &chatRequestFilterError{
		status:  http.StatusBadRequest,
		message: err.Error(),
		wrapped: err,
	}
}

type ChatRequestPipeline struct {
	parameters VLLMParameterCatalog
	messages   ChatMessageProcessor
}

func defaultChatRequestPipeline() ChatRequestPipeline {
	return ChatRequestPipeline{
		parameters: defaultParameterCatalog,
		messages:   defaultMessageProcessor,
	}
}

// Normalize runs the catalog (generic + per-model rules) and emits the rewritten body.
// routedModel is the proxy's fallback used when body.model is missing.
func (p ChatRequestPipeline) Normalize(body []byte, adminAuthenticated bool, limits outputTokenLimits, routedModel string) ([]byte, chatRequest, error) {
	ctx, err := newRequestFilterContext(body, adminAuthenticated, limits)
	if err != nil {
		return nil, chatRequest{}, err
	}
	ctx.ResolveRoutedModel(routedModel)
	if err := p.parameters.Apply(RequestFilterStagePreValidation, ctx); err != nil {
		return nil, chatRequest{}, err
	}
	if err := p.messages.ValidateDocument(&ctx.Document); err != nil {
		return nil, chatRequest{}, err
	}
	if err := ctx.DecodeRequest(); err != nil {
		return nil, chatRequest{}, err
	}
	p.applyOutputTokenLimits(ctx)
	if err := p.parameters.Apply(RequestFilterStagePostLimits, ctx); err != nil {
		return nil, chatRequest{}, err
	}
	if err := ctx.SyncRequestView(); err != nil {
		return nil, chatRequest{}, err
	}
	updatedBody, err := ctx.Document.Marshal()
	if err != nil {
		return nil, chatRequest{}, err
	}
	return updatedBody, ctx.Request, nil
}

func (p ChatRequestPipeline) applyOutputTokenLimits(ctx *RequestFilterContext) {
	_, hasMaxTokens := ctx.Document.Get("max_tokens")
	_, hasMaxCompletionTokens := ctx.Document.Get("max_completion_tokens")
	limits := normalizedOutputTokenLimits(ctx.OutputLimits)

	switch {
	case hasMaxTokens && hasMaxCompletionTokens:
		maxTokens := capOutputTokens(ctx.Request.MaxTokens, true, ctx.AdminAuthenticated, limits)
		maxCompletionTokens := capOutputTokens(ctx.Request.MaxCompletionTokens, true, ctx.AdminAuthenticated, limits)
		if maxCompletionTokens < maxTokens {
			maxTokens = maxCompletionTokens
		} else {
			maxCompletionTokens = maxTokens
		}
		ctx.Document.Set("max_tokens", maxTokens)
		ctx.Document.Set("max_completion_tokens", maxCompletionTokens)
		ctx.Request.MaxTokens = maxTokens
		ctx.Request.MaxCompletionTokens = maxCompletionTokens
	case hasMaxTokens:
		maxTokens := capOutputTokens(ctx.Request.MaxTokens, true, ctx.AdminAuthenticated, limits)
		ctx.Document.Set("max_tokens", maxTokens)
		ctx.Request.MaxTokens = maxTokens
		ctx.Request.MaxCompletionTokens = 0
	case hasMaxCompletionTokens:
		maxCompletionTokens := capOutputTokens(ctx.Request.MaxCompletionTokens, true, ctx.AdminAuthenticated, limits)
		ctx.Document.Set("max_completion_tokens", maxCompletionTokens)
		ctx.Request.MaxCompletionTokens = maxCompletionTokens
		ctx.Request.MaxTokens = maxCompletionTokens
	default:
		maxTokens := capOutputTokens(0, false, ctx.AdminAuthenticated, limits)
		ctx.Document.Set("max_tokens", maxTokens)
		ctx.Request.MaxTokens = maxTokens
		ctx.Request.MaxCompletionTokens = 0
	}
}

func readLimitedChatRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, badChatRequest("request body is required")
	}
	defer r.Body.Close()

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxChatRequestBodySize+1))
	if err != nil {
		return nil, badChatRequest("read body: %v", err)
	}
	if len(body) > MaxChatRequestBodySize {
		return nil, &chatRequestFilterError{status: http.StatusRequestEntityTooLarge, message: "request body too large"}
	}
	return body, nil
}

func prepareChatRequestBody(r *http.Request) ([]byte, chatRequest, error) {
	return prepareChatRequestBodyWithTokenLimits(r, defaultOutputTokenLimits(), "")
}

func prepareChatRequestBodyWithTokenLimits(r *http.Request, limits outputTokenLimits, routedModel string) ([]byte, chatRequest, error) {
	body, err := readLimitedChatRequestBody(r)
	if err != nil {
		return nil, chatRequest{}, err
	}
	body, err = normalizeContent(body)
	if err != nil {
		return nil, chatRequest{}, err
	}
	return normalizeChatRequestForAuthAndLimits(body, requestHasAdminAuth(r), limits, routedModel)
}

func normalizeChatRequest(body []byte) ([]byte, chatRequest, error) {
	return normalizeChatRequestForAuthAndLimits(body, false, defaultOutputTokenLimits(), "")
}

func normalizeChatRequestForModel(body []byte, routedModel string) ([]byte, chatRequest, error) {
	return normalizeChatRequestForAuthAndLimits(body, false, defaultOutputTokenLimits(), routedModel)
}

func normalizeChatRequestForAuthAndLimits(body []byte, adminAuthenticated bool, limits outputTokenLimits, routedModel string) ([]byte, chatRequest, error) {
	return defaultChatRequestPipeline().Normalize(body, adminAuthenticated, limits, routedModel)
}

func defaultOutputTokenLimits() outputTokenLimits {
	return outputTokenLimits{DefaultMaxTokens: DefaultRequestMaxTokens, MaxTokensCap: RequestMaxTokensCap}
}

func normalizedOutputTokenLimits(limits outputTokenLimits) outputTokenLimits {
	if limits.DefaultMaxTokens == 0 {
		limits.DefaultMaxTokens = DefaultRequestMaxTokens
	}
	if limits.MaxTokensCap == 0 {
		limits.MaxTokensCap = RequestMaxTokensCap
	}
	return limits
}

func capOutputTokens(value uint64, explicitlySet bool, bypassLimit bool, limits outputTokenLimits) uint64 {
	limits = normalizedOutputTokenLimits(limits)
	if value == 0 {
		return limits.DefaultMaxTokens
	}
	if explicitlySet && !bypassLimit && limits.MaxTokensCap > 0 && value > limits.MaxTokensCap {
		return limits.MaxTokensCap
	}
	return value
}

func unsupportedChatParameterMessage(name string) string {
	return fmt.Sprintf("feature %q is temporarily unavailable", name)
}

func numericJSONValueAsUint64(value any) (uint64, bool) {
	switch v := value.(type) {
	case float64:
		if v < 0 || v != float64(uint64(v)) {
			return 0, false
		}
		return uint64(v), true
	case uint64:
		return v, true
	case int:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case int64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil || n < 0 {
			return 0, false
		}
		return uint64(n), true
	default:
		return 0, false
	}
}
