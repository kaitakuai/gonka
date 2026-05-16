package main

import (
	"bytes"
	"encoding/json"
	"math"
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
	RequestFilterStagePreValidation RequestFilterStage = iota
	RequestFilterStagePostLimits
)

type ParameterSupport int

const (
	ParameterSupportPassthrough ParameterSupport = iota
	ParameterSupportStrip
	ParameterSupportReject
	ParameterSupportSanitize
	ParameterSupportForce
)

type VLLMParameter struct {
	Name     string
	Category ParameterCategory
	Stage    RequestFilterStage
	Support  ParameterSupport
	Handler  ParameterHandler
}

type ParameterHandler interface {
	Apply(*RequestFilterContext, VLLMParameter) error
}

type NoopParameterHandler struct{}

func (NoopParameterHandler) Apply(_ *RequestFilterContext, _ VLLMParameter) error { return nil }

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

type SanitizeFloatParameterHandler struct {
	StripNonFinite bool
	Max            *float64
}

func (h SanitizeFloatParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	value, ok := ctx.Document.Get(parameter.Name)
	if !ok {
		return nil
	}
	if stringValue, ok := value.(string); ok {
		if h.StripNonFinite && isNonFiniteString(stringValue) {
			ctx.Document.Delete(parameter.Name)
		}
		return nil
	}
	number, ok := numericJSONValueAsFloat64(value)
	if !ok {
		return nil
	}
	if h.StripNonFinite && (math.IsNaN(number) || math.IsInf(number, 0)) {
		ctx.Document.Delete(parameter.Name)
		return nil
	}
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

func defaultVLLMParameterCatalog() VLLMParameterCatalog {
	return VLLMParameterCatalog{parameters: []VLLMParameter{
		newPassthroughParameter("model", ParameterCategoryString),
		newPassthroughParameter("stream", ParameterCategoryBool),
		newPassthroughParameter("messages", ParameterCategoryObjectArray),
		newPassthroughParameter("max_tokens", ParameterCategoryIntRange),
		newPassthroughParameter("max_completion_tokens", ParameterCategoryIntRange),
		newPassthroughParameter("min_tokens", ParameterCategoryIntRange),
		newPassthroughParameter("n", ParameterCategoryIntRange),
		newPassthroughParameter("temperature", ParameterCategoryFloatRange),
		newPassthroughParameter("top_p", ParameterCategoryFloatRange),
		newPassthroughParameter("top_k", ParameterCategoryIntRange),
		newPassthroughParameter("min_p", ParameterCategoryFloatRange),
		newPassthroughParameter("repetition_penalty", ParameterCategoryFloatRange),
		newPassthroughParameter("logit_bias", ParameterCategoryIntToFloat),
		newPassthroughParameter("bad_words", ParameterCategoryStringList),
		newPassthroughParameter("stop", ParameterCategoryStringList),
		newPassthroughParameter("stop_token_ids", ParameterCategoryIntList),
		newPassthroughParameter("allowed_token_ids", ParameterCategoryIntList),
		newPassthroughParameter("seed", ParameterCategoryIntRange),
		newPassthroughParameter("ignore_eos", ParameterCategoryBool),
		newPassthroughParameter("skip_special_tokens", ParameterCategoryBool),
		newPassthroughParameter("detokenize", ParameterCategoryBool),
		newPassthroughParameter("thinking", ParameterCategoryObject),
		newPassthroughParameter("chat_template_kwargs", ParameterCategoryObject),
		newPassthroughParameter("tools", ParameterCategoryObjectArray),
		newPassthroughParameter("tool_choice", ParameterCategoryString),
		newPassthroughParameter("response_format", ParameterCategoryObject),
		newStrippedParameter("presence_penalty", ParameterCategoryFloatRange, RequestFilterStagePreValidation),
		newStrippedParameter("frequency_penalty", ParameterCategoryFloatRange, RequestFilterStagePreValidation),
		newStrippedParameter("structured_outputs", ParameterCategoryObject, RequestFilterStagePreValidation),
		newStrippedParameter("prompt_logprobs", ParameterCategoryIntRange, RequestFilterStagePreValidation),
		newStrippedParameter("use_beam_search", ParameterCategoryBool, RequestFilterStagePreValidation),
		newStrippedParameter("truncate_prompt_tokens", ParameterCategoryIntRange, RequestFilterStagePreValidation),
		newConditionalParameter("min_tokens", ParameterCategoryIntRange, RequestFilterStagePreValidation, ParameterSupportStrip, ConditionalStripParameterHandler{
			Predicate: func(ctx *RequestFilterContext) bool {
				return ctx.Document.Has("stop_token_ids")
			},
		}),
		newSanitizedParameter("bad_words", ParameterCategoryStringList, RequestFilterStagePreValidation, SanitizeStringListParameterHandler{
			Keep: func(value string) bool {
				return strings.TrimSpace(value) != ""
			},
			DropFieldIfEmpty: true,
		}),
		newSanitizedParameter("tools", ParameterCategoryObjectArray, RequestFilterStagePreValidation, CustomParameterHandler{ApplyFunc: stripEmptyToolsAndToolChoice}),
		newRejectedParameter("enforced_tokens", ParameterCategoryAny, RequestFilterStagePreValidation, unsupportedChatParameterMessage("enforced_tokens")),
		newRejectedNestedStringValueParameter("response_format", "type", "json_object", ParameterCategoryObject, RequestFilterStagePreValidation, unsupportedChatParameterMessage("response_format.type=json_object")),
		newRejectedNestedStringValueParameter("response_format", "type", "json_schema", ParameterCategoryObject, RequestFilterStagePreValidation, unsupportedChatParameterMessage("response_format.type=json_schema")),
		newRejectedParameter("guided_regex", ParameterCategoryAny, RequestFilterStagePreValidation, unsupportedChatParameterMessage("guided_regex")),
		newRejectedParameter("guided_grammar", ParameterCategoryAny, RequestFilterStagePreValidation, unsupportedChatParameterMessage("guided_grammar")),
		newRejectedParameter("guided_json", ParameterCategoryAny, RequestFilterStagePreValidation, unsupportedChatParameterMessage("guided_json")),
		newRejectedParameter("guided_choice", ParameterCategoryAny, RequestFilterStagePreValidation, unsupportedChatParameterMessage("guided_choice")),
		newRejectedStringValueParameter("tool_choice", "required", ParameterCategoryString, RequestFilterStagePreValidation, unsupportedChatParameterMessage("tool_choice=required")),
		newSanitizedParameter("structured_outputs.regex", ParameterCategoryObject, RequestFilterStagePreValidation, CustomParameterHandler{ApplyFunc: rejectStructuredOutputsRegex}),
		newSanitizedParameter("n", ParameterCategoryIntRange, RequestFilterStagePostLimits, CapUintParameterHandler{Max: MaxChatRequestChoices}),
		newSanitizedParameter("min_tokens", ParameterCategoryIntRange, RequestFilterStagePostLimits, ClampUintToFieldParameterHandler{MaxField: "max_tokens"}),
		newSanitizedParameter("temperature", ParameterCategoryFloatRange, RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true, Max: floatPointer(MaxTemperature)}),
		newSanitizedParameter("top_p", ParameterCategoryFloatRange, RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newSanitizedParameter("top_k", ParameterCategoryIntRange, RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newSanitizedParameter("min_p", ParameterCategoryFloatRange, RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newSanitizedParameter("repetition_penalty", ParameterCategoryFloatRange, RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newSanitizedParameter("logit_bias", ParameterCategoryIntToFloat, RequestFilterStagePostLimits, SanitizeFloatMapParameterHandler{StripNonFinite: true, Min: floatPointer(-100), Max: floatPointer(100), DropFieldIfEmpty: true}),
		newForcedParameter("logprobs", ParameterCategoryBool, RequestFilterStagePostLimits, true),
		newForcedParameter("top_logprobs", ParameterCategoryIntRange, RequestFilterStagePostLimits, 5),
	}}
}

func (c VLLMParameterCatalog) Apply(stage RequestFilterStage, ctx *RequestFilterContext) error {
	for _, parameter := range c.parameters {
		if parameter.Stage != stage || parameter.Handler == nil {
			continue
		}
		if err := parameter.Handler.Apply(ctx, parameter); err != nil {
			return err
		}
	}
	return nil
}

func newPassthroughParameter(name string, category ParameterCategory) VLLMParameter {
	return VLLMParameter{Name: name, Category: category, Support: ParameterSupportPassthrough, Handler: NoopParameterHandler{}}
}

func newStrippedParameter(name string, category ParameterCategory, stage RequestFilterStage) VLLMParameter {
	return VLLMParameter{Name: name, Category: category, Stage: stage, Support: ParameterSupportStrip, Handler: StripParameterHandler{}}
}

func newRejectedParameter(name string, category ParameterCategory, stage RequestFilterStage, message string) VLLMParameter {
	return VLLMParameter{Name: name, Category: category, Stage: stage, Support: ParameterSupportReject, Handler: RejectParameterHandler{Message: message}}
}

func newRejectedStringValueParameter(name string, value string, category ParameterCategory, stage RequestFilterStage, message string) VLLMParameter {
	return VLLMParameter{Name: name, Category: category, Stage: stage, Support: ParameterSupportReject, Handler: RejectStringValueParameterHandler{Value: value, Message: message}}
}

func newRejectedNestedStringValueParameter(parent string, field string, value string, category ParameterCategory, stage RequestFilterStage, message string) VLLMParameter {
	return VLLMParameter{Name: parent + "." + field, Category: category, Stage: stage, Support: ParameterSupportReject, Handler: RejectNestedStringValueParameterHandler{Parent: parent, Field: field, Value: value, Message: message}}
}

func newSanitizedParameter(name string, category ParameterCategory, stage RequestFilterStage, handler ParameterHandler) VLLMParameter {
	return VLLMParameter{Name: name, Category: category, Stage: stage, Support: ParameterSupportSanitize, Handler: handler}
}

func newForcedParameter(name string, category ParameterCategory, stage RequestFilterStage, value any) VLLMParameter {
	return VLLMParameter{Name: name, Category: category, Stage: stage, Support: ParameterSupportForce, Handler: ForceLiteralParameterHandler{Value: value}}
}

func newConditionalParameter(name string, category ParameterCategory, stage RequestFilterStage, support ParameterSupport, handler ParameterHandler) VLLMParameter {
	return VLLMParameter{Name: name, Category: category, Stage: stage, Support: support, Handler: handler}
}

func stripEmptyToolsAndToolChoice(ctx *RequestFilterContext, _ VLLMParameter) error {
	tools, ok := ctx.Document.Array("tools")
	if ok && len(tools) == 0 {
		ctx.Document.Delete("tools")
		ctx.Document.Delete("tool_choice")
	}
	return nil
}

func rejectStructuredOutputsRegex(ctx *RequestFilterContext, _ VLLMParameter) error {
	structuredOutputs, ok := ctx.Document.Object("structured_outputs")
	if !ok {
		return nil
	}
	if _, exists := structuredOutputs["regex"]; exists {
		return badChatRequest("%s", unsupportedChatParameterMessage("structured_outputs.regex"))
	}
	return nil
}

func isNonFiniteString(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return lower == "nan" || lower == "inf" || lower == "+inf" || lower == "-inf" || lower == "infinity" || lower == "+infinity" || lower == "-infinity"
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
