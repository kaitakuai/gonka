package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const MaxChatRequestBodySize = 10 * 1024 * 1024
const MaxChatRequestChoices = 5
const MaxTemperature = 2.0
const kimiK26ModelID = "moonshotai/Kimi-K2.6"
const emptyToolResultContent = "<empty tool result>"

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
}

func (e *chatRequestFilterError) Error() string {
	return e.message
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

type ChatRequestPipeline struct {
	parameters VLLMParameterCatalog
	messages   ChatMessageProcessor
}

func defaultChatRequestPipeline() ChatRequestPipeline {
	return ChatRequestPipeline{
		parameters: defaultVLLMParameterCatalog(),
		messages:   defaultChatMessageProcessor(),
	}
}

func (p ChatRequestPipeline) Normalize(body []byte, adminAuthenticated bool, limits outputTokenLimits) ([]byte, chatRequest, error) {
	ctx, err := newRequestFilterContext(body, adminAuthenticated, limits)
	if err != nil {
		return nil, chatRequest{}, err
	}
	if err := p.parameters.Apply(RequestFilterStagePreValidation, ctx); err != nil {
		return nil, chatRequest{}, err
	}
	if err := p.messages.ValidateDocument(ctx.Document); err != nil {
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

func (p ChatRequestPipeline) ApplyModelOverrides(body []byte, req chatRequest, model string) ([]byte, chatRequest, error) {
	if model != kimiK26ModelID {
		return body, req, nil
	}
	ctx, err := newRequestFilterContext(body, false, defaultOutputTokenLimits())
	if err != nil {
		return nil, chatRequest{}, err
	}
	translateKimiThinkingForVLLM(ctx.Document.raw)
	if err := p.parameters.Apply(RequestFilterStagePreValidation, ctx); err != nil {
		return nil, chatRequest{}, err
	}
	updatedBody, err := ctx.Document.Marshal()
	if err != nil {
		return nil, chatRequest{}, err
	}
	return updatedBody, req, nil
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
	return prepareChatRequestBodyWithTokenLimits(r, defaultOutputTokenLimits())
}

func prepareChatRequestBodyWithTokenLimits(r *http.Request, limits outputTokenLimits) ([]byte, chatRequest, error) {
	body, err := readLimitedChatRequestBody(r)
	if err != nil {
		return nil, chatRequest{}, err
	}
	body, err = normalizeContent(body)
	if err != nil {
		return nil, chatRequest{}, err
	}
	return normalizeChatRequestForAuthAndLimits(body, requestHasAdminAuth(r), limits)
}

func normalizeChatRequest(body []byte) ([]byte, chatRequest, error) {
	return normalizeChatRequestForAuth(body, false)
}

func normalizeChatRequestForAuth(body []byte, adminAuthenticated bool) ([]byte, chatRequest, error) {
	return normalizeChatRequestForAuthAndLimits(body, adminAuthenticated, defaultOutputTokenLimits())
}

func normalizeChatRequestForAuthAndLimits(body []byte, adminAuthenticated bool, limits outputTokenLimits) ([]byte, chatRequest, error) {
	return defaultChatRequestPipeline().Normalize(body, adminAuthenticated, limits)
}

func applyKimiRequestOverrides(body []byte, req chatRequest, model string) ([]byte, chatRequest, error) {
	return defaultChatRequestPipeline().ApplyModelOverrides(body, req, model)
}

func translateKimiThinkingForVLLM(request map[string]any) {
	enabled, ok := moonshotThinkingEnabled(request["thinking"])
	if !ok {
		return
	}
	chatTemplateKwargs, _ := request["chat_template_kwargs"].(map[string]any)
	if chatTemplateKwargs == nil {
		chatTemplateKwargs = map[string]any{}
		request["chat_template_kwargs"] = chatTemplateKwargs
	}
	if _, exists := chatTemplateKwargs["thinking"]; exists {
		return
	}
	chatTemplateKwargs["thinking"] = enabled
}

func moonshotThinkingEnabled(value any) (bool, bool) {
	thinking, ok := value.(map[string]any)
	if !ok {
		return false, false
	}
	typ, ok := thinking["type"].(string)
	if !ok {
		return false, false
	}
	switch strings.ToLower(typ) {
	case "enabled":
		return true, true
	case "disabled":
		return false, true
	default:
		return false, false
	}
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
	return fmt.Sprintf("parameter %q is unsupported", name)
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
