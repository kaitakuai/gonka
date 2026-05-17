package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

type ParameterCategory int

const (
	ParameterCategoryAny ParameterCategory = iota
	ParameterCategoryString
	ParameterCategoryBool
	ParameterCategoryStringList
	ParameterCategoryIntList
	ParameterCategoryFloatRange
	ParameterCategoryIntRange
	ParameterCategoryIntToFloat
	ParameterCategoryObject
	ParameterCategoryObjectArray
)

type RequestFilterStage int

const (
	// PreValidation rules run on the raw request document before we decode and validate it.
	RequestFilterStagePreValidation RequestFilterStage = iota
	// PostLimits rules run after max token defaults/caps are resolved back into the document.
	RequestFilterStagePostLimits
)

type ParameterSupport int

const (
	ParameterSupportPassthrough ParameterSupport = iota
	// Strip drops an unsupported field but keeps the request itself valid.
	ParameterSupportStrip
	// Reject fails the request because forwarding it would violate the upstream contract.
	ParameterSupportReject
	// Sanitize rewrites a field into an allowed shape or value range.
	ParameterSupportSanitize
	// Force overwrites the field with a gateway-owned value.
	ParameterSupportForce
)

// ParameterRule describes one transformation for a field at a specific pipeline stage.
type ParameterRule struct {
	Stage   RequestFilterStage
	Support ParameterSupport
	Handler ParameterHandler
}

type VLLMParameter struct {
	Name     string
	Category ParameterCategory
	Rules    []ParameterRule
}

type ParameterHandler interface {
	Apply(*RequestFilterContext, VLLMParameter) error
}

type StripParameterHandler struct{}

func (StripParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	ctx.Document.Delete(parameter.Name)
	return nil
}

type ConditionalStripParameterHandler struct {
	Predicate func(*RequestFilterContext) bool
}

func (h ConditionalStripParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	if h.Predicate != nil && h.Predicate(ctx) {
		ctx.Document.Delete(parameter.Name)
	}
	return nil
}

type RejectParameterHandler struct {
	Message string
}

func (h RejectParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	if ctx.Document.Has(parameter.Name) {
		return badChatRequest("%s", h.Message)
	}
	return nil
}

type RejectStringValueParameterHandler struct {
	Value   string
	Message string
}

func (h RejectStringValueParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	current, ok := ctx.Document.String(parameter.Name)
	if ok && current == h.Value {
		return badChatRequest("%s", h.Message)
	}
	return nil
}

type RejectNestedStringValueParameterHandler struct {
	Parent  string
	Field   string
	Value   string
	Message string
}

func (h RejectNestedStringValueParameterHandler) Apply(ctx *RequestFilterContext, _ VLLMParameter) error {
	parent, ok := ctx.Document.Object(h.Parent)
	if !ok {
		return nil
	}
	current, ok := parent[h.Field].(string)
	if ok && current == h.Value {
		return badChatRequest("%s", h.Message)
	}
	return nil
}

type SanitizeStringListParameterHandler struct {
	Keep             func(string) bool
	DropFieldIfEmpty bool
}

func (h SanitizeStringListParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	raw, ok := ctx.Document.Array(parameter.Name)
	if !ok {
		return nil
	}
	cleaned := raw[:0]
	for _, item := range raw {
		value, ok := item.(string)
		if !ok {
			cleaned = append(cleaned, item)
			continue
		}
		if h.Keep == nil || h.Keep(value) {
			cleaned = append(cleaned, value)
		}
	}
	if len(cleaned) == 0 && h.DropFieldIfEmpty {
		ctx.Document.Delete(parameter.Name)
		return nil
	}
	ctx.Document.Set(parameter.Name, cleaned)
	return nil
}

// SanitizeFloatParameterHandler normalizes numeric knobs from either JSON numbers or string-encoded numbers.
type SanitizeFloatParameterHandler struct {
	StripNonFinite bool
	Max            *float64
}

func (h SanitizeFloatParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	value, ok := ctx.Document.Get(parameter.Name)
	if !ok {
		return nil
	}
	number, ok := numericJSONValueAsFloat64(value)
	if !ok {
		ctx.Document.Delete(parameter.Name)
		return nil
	}
	if h.StripNonFinite && (math.IsNaN(number) || math.IsInf(number, 0)) {
		ctx.Document.Delete(parameter.Name)
		return nil
	}
	ctx.Document.Set(parameter.Name, number)
	if h.Max != nil && number > *h.Max {
		ctx.Document.Set(parameter.Name, *h.Max)
	}
	return nil
}

type SanitizeFloatMapParameterHandler struct {
	StripNonFinite   bool
	Min              *float64
	Max              *float64
	DropFieldIfEmpty bool
}

func (h SanitizeFloatMapParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	raw, ok := ctx.Document.Object(parameter.Name)
	if !ok {
		return nil
	}
	for key, value := range raw {
		number, ok := numericJSONValueAsFloat64(value)
		if !ok {
			continue
		}
		if h.StripNonFinite && (math.IsNaN(number) || math.IsInf(number, 0)) {
			delete(raw, key)
			continue
		}
		if h.Min != nil && number < *h.Min {
			delete(raw, key)
			continue
		}
		if h.Max != nil && number > *h.Max {
			delete(raw, key)
		}
	}
	if len(raw) == 0 && h.DropFieldIfEmpty {
		ctx.Document.Delete(parameter.Name)
		return nil
	}
	ctx.Document.Set(parameter.Name, raw)
	return nil
}

type ForceLiteralParameterHandler struct {
	Value any
}

func (h ForceLiteralParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	ctx.Document.Set(parameter.Name, h.Value)
	return nil
}

type CapUintParameterHandler struct {
	Max uint64
}

func (h CapUintParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	value, ok := numericJSONValueAsUint64FromDocument(ctx.Document, parameter.Name)
	if !ok {
		return nil
	}
	if value > h.Max {
		ctx.Document.Set(parameter.Name, h.Max)
	}
	return nil
}

type ClampUintToFieldParameterHandler struct {
	MaxField string
}

func (h ClampUintToFieldParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	value, ok := numericJSONValueAsUint64FromDocument(ctx.Document, parameter.Name)
	if !ok {
		return nil
	}
	maxValue, ok := numericJSONValueAsUint64FromDocument(ctx.Document, h.MaxField)
	if !ok || maxValue == 0 {
		return nil
	}
	if value > maxValue {
		ctx.Document.Set(parameter.Name, maxValue)
	}
	return nil
}

type CustomParameterHandler struct {
	ApplyFunc func(*RequestFilterContext, VLLMParameter) error
}

func (h CustomParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	if h.ApplyFunc == nil {
		return nil
	}
	return h.ApplyFunc(ctx, parameter)
}

type ChatRequestDocument struct {
	raw map[string]any
}

func (d *ChatRequestDocument) Keys() []string {
	keys := make([]string, 0, len(d.raw))
	for key := range d.raw {
		keys = append(keys, key)
	}
	return keys
}

func decodeChatRequestDocument(body []byte) (*ChatRequestDocument, error) {
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, badChatRequest("parse request: %v", err)
	}
	return &ChatRequestDocument{raw: raw}, nil
}

func (d *ChatRequestDocument) Marshal() ([]byte, error) {
	updatedBody, err := json.Marshal(d.raw)
	if err != nil {
		return nil, badChatRequest("marshal request: %v", err)
	}
	return updatedBody, nil
}

func (d *ChatRequestDocument) DecodeInto(dst any) error {
	body, err := d.Marshal()
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return badChatRequest("parse request: %v", err)
	}
	return nil
}

func (d *ChatRequestDocument) Has(name string) bool {
	_, ok := d.raw[name]
	return ok
}

func (d *ChatRequestDocument) Get(name string) (any, bool) {
	value, ok := d.raw[name]
	return value, ok
}

func (d *ChatRequestDocument) Set(name string, value any) {
	d.raw[name] = value
}

func (d *ChatRequestDocument) Delete(name string) {
	delete(d.raw, name)
}

func (d *ChatRequestDocument) String(name string) (string, bool) {
	value, ok := d.raw[name].(string)
	return value, ok
}

func (d *ChatRequestDocument) Object(name string) (map[string]any, bool) {
	value, ok := d.raw[name].(map[string]any)
	return value, ok
}

func (d *ChatRequestDocument) Array(name string) ([]any, bool) {
	value, ok := d.raw[name].([]any)
	return value, ok
}

type RequestFilterContext struct {
	Document           *ChatRequestDocument
	OutputLimits       outputTokenLimits
	AdminAuthenticated bool
	Request            chatRequest
}

func newRequestFilterContext(body []byte, adminAuthenticated bool, limits outputTokenLimits) (*RequestFilterContext, error) {
	document, err := decodeChatRequestDocument(body)
	if err != nil {
		return nil, err
	}
	return &RequestFilterContext{
		Document:           document,
		OutputLimits:       normalizedOutputTokenLimits(limits),
		AdminAuthenticated: adminAuthenticated,
	}, nil
}

func (ctx *RequestFilterContext) DecodeRequest() error {
	var req chatRequest
	if err := ctx.Document.DecodeInto(&req); err != nil {
		return err
	}
	ctx.Request = req
	return nil
}

func (ctx *RequestFilterContext) SyncRequestView() error {
	var req chatRequest
	if err := ctx.Document.DecodeInto(&req); err != nil {
		return err
	}
	req.MaxTokens = ctx.Request.MaxTokens
	req.MaxCompletionTokens = ctx.Request.MaxCompletionTokens
	ctx.Request = req
	return nil
}

type VLLMParameterCatalog struct {
	parameters []VLLMParameter
}

var defaultParameterCatalog = defaultVLLMParameterCatalog()

// The catalog is the single source of truth for how each supported OpenAI/vLLM field is treated.
func defaultVLLMParameterCatalog() VLLMParameterCatalog {
	return VLLMParameterCatalog{parameters: []VLLMParameter{
		newParameter("model", ParameterCategoryString),
		newParameter("stream", ParameterCategoryBool),
		newParameter("messages", ParameterCategoryObjectArray),
		newParameter("max_tokens", ParameterCategoryIntRange),
		newParameter("max_completion_tokens", ParameterCategoryIntRange),
		newParameter("n", ParameterCategoryIntRange).
			withSanitizeRule(RequestFilterStagePostLimits, CapUintParameterHandler{Max: MaxChatRequestChoices}),
		newParameter("temperature", ParameterCategoryFloatRange).
			withSanitizeRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true, Max: floatPointer(MaxTemperature)}),
		newParameter("top_p", ParameterCategoryFloatRange).
			withSanitizeRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newParameter("top_k", ParameterCategoryIntRange).
			withSanitizeRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newParameter("min_p", ParameterCategoryFloatRange).
			withSanitizeRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newParameter("repetition_penalty", ParameterCategoryFloatRange).
			withSanitizeRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true, Max: floatPointer(MaxRepetitionPenalty)}),
		newParameter("logit_bias", ParameterCategoryIntToFloat).
			withSanitizeRule(RequestFilterStagePostLimits, SanitizeFloatMapParameterHandler{StripNonFinite: true, Min: floatPointer(-100), Max: floatPointer(100), DropFieldIfEmpty: true}),
		newParameter("stop", ParameterCategoryStringList),
		newParameter("stop_token_ids", ParameterCategoryIntList),
		newParameter("seed", ParameterCategoryIntRange),
		newParameter("skip_special_tokens", ParameterCategoryBool),
		newParameter("detokenize", ParameterCategoryBool),
		newParameter("thinking", ParameterCategoryObject),
		newParameter("chat_template_kwargs", ParameterCategoryObject),
		newParameter("tool_choice", ParameterCategoryString).
			withRejectRule(RequestFilterStagePreValidation, RejectStringValueParameterHandler{Value: "required", Message: unsupportedChatParameterMessage("tool_choice=required")}),
		newParameter("min_tokens", ParameterCategoryIntRange).
			withConditionalStripRule(RequestFilterStagePreValidation, ConditionalStripParameterHandler{
				Predicate: func(ctx *RequestFilterContext) bool {
					return ctx.Document.Has("stop_token_ids")
				},
			}).
			withSanitizeRule(RequestFilterStagePostLimits, ClampUintToFieldParameterHandler{MaxField: "max_tokens"}),
		newParameter("bad_words", ParameterCategoryStringList).
			withSanitizeRule(RequestFilterStagePreValidation, SanitizeStringListParameterHandler{
				Keep: func(value string) bool {
					return strings.TrimSpace(value) != ""
				},
				DropFieldIfEmpty: true,
			}),
		newParameter("tools", ParameterCategoryObjectArray).
			withSanitizeRule(RequestFilterStagePreValidation, CustomParameterHandler{ApplyFunc: stripEmptyToolsAndToolChoice}),
		newParameter("logprobs", ParameterCategoryBool).
			withForceRule(RequestFilterStagePostLimits, ForceLiteralParameterHandler{Value: true}),
		newParameter("top_logprobs", ParameterCategoryIntRange).
			withForceRule(RequestFilterStagePostLimits, ForceLiteralParameterHandler{Value: 5}),
	}}
}

func (c VLLMParameterCatalog) Apply(stage RequestFilterStage, ctx *RequestFilterContext) error {
	if stage == RequestFilterStagePreValidation {
		if err := c.rejectUnknownParameters(ctx); err != nil {
			return err
		}
	}
	for _, parameter := range c.parameters {
		for _, rule := range parameter.Rules {
			if rule.Stage != stage || rule.Handler == nil {
				continue
			}
			if err := rule.Handler.Apply(ctx, parameter); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c VLLMParameterCatalog) rejectUnknownParameters(ctx *RequestFilterContext) error {
	if ctx == nil || ctx.Document == nil {
		return nil
	}
	known := make(map[string]struct{}, len(c.parameters))
	for _, parameter := range c.parameters {
		known[parameter.Name] = struct{}{}
	}
	for _, key := range ctx.Document.Keys() {
		if _, ok := known[key]; ok {
			continue
		}
		return badChatRequest("%s", unsupportedChatParameterMessage(key))
	}
	return nil
}

func newParameter(name string, category ParameterCategory) VLLMParameter {
	return VLLMParameter{Name: name, Category: category}
}

func (p VLLMParameter) appendRule(stage RequestFilterStage, support ParameterSupport, handler ParameterHandler) VLLMParameter {
	p.Rules = append(p.Rules, ParameterRule{Stage: stage, Support: support, Handler: handler})
	return p
}

func (p VLLMParameter) withStripRule(stage RequestFilterStage) VLLMParameter {
	return p.appendRule(stage, ParameterSupportStrip, StripParameterHandler{})
}

func (p VLLMParameter) withConditionalStripRule(stage RequestFilterStage, handler ConditionalStripParameterHandler) VLLMParameter {
	return p.appendRule(stage, ParameterSupportStrip, handler)
}

func (p VLLMParameter) withRejectRule(stage RequestFilterStage, handler ParameterHandler) VLLMParameter {
	return p.appendRule(stage, ParameterSupportReject, handler)
}

func (p VLLMParameter) withSanitizeRule(stage RequestFilterStage, handler ParameterHandler) VLLMParameter {
	return p.appendRule(stage, ParameterSupportSanitize, handler)
}

func (p VLLMParameter) withForceRule(stage RequestFilterStage, handler ParameterHandler) VLLMParameter {
	return p.appendRule(stage, ParameterSupportForce, handler)
}

func stripEmptyToolsAndToolChoice(ctx *RequestFilterContext, _ VLLMParameter) error {
	tools, ok := ctx.Document.Array("tools")
	if ok && len(tools) == 0 {
		ctx.Document.Delete("tools")
		ctx.Document.Delete("tool_choice")
	}
	return nil
}

func floatPointer(value float64) *float64 {
	return &value
}

func numericJSONValueAsFloat64(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case json.Number:
		number, err := v.Float64()
		return number, err == nil
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func numericJSONValueAsUint64FromDocument(document *ChatRequestDocument, field string) (uint64, bool) {
	value, ok := document.Get(field)
	if !ok {
		return 0, false
	}
	return numericJSONValueAsUint64(value)
}
