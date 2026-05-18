package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"devshard/cmd/devshardctl/paramvalidators"
)

type RequestFilterStage int

const (
	// PreValidation rules run on the raw request document before we decode and validate it.
	RequestFilterStagePreValidation RequestFilterStage = iota
	// PostLimits rules run after max token defaults/caps are resolved back into the document.
	RequestFilterStagePostLimits
)

// ParameterRule describes one transformation for a field at a specific pipeline stage.
type ParameterRule struct {
	Stage   RequestFilterStage
	Handler ParameterHandler
}

type VLLMParameter struct {
	Name  string
	Rules []ParameterRule
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
	MaxEntries       int
}

func (h SanitizeFloatMapParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	raw, ok := ctx.Document.Object(parameter.Name)
	if !ok {
		return nil
	}
	if h.MaxEntries > 0 && len(raw) > h.MaxEntries {
		return badChatRequest("%s: map size %d exceeds limit %d", parameter.Name, len(raw), h.MaxEntries)
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

// ValidateUintParameterHandler rejects the request if the field is present but its value
// cannot be parsed as a non-negative integer that fits in uint64. Pass-through when the
// field is absent. Used for fields like `seed` where vLLM expects a uint64 and we want to
// catch garbage types at the gateway boundary rather than relying on the upstream's error
// path.
type ValidateUintParameterHandler struct{}

func (h ValidateUintParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	raw, exists := ctx.Document.Get(parameter.Name)
	if !exists || raw == nil {
		return nil
	}
	if _, ok := numericJSONValueAsUint64(raw); !ok {
		return badChatRequest("%s: must be a non-negative integer", parameter.Name)
	}
	return nil
}

// LengthCapListParameterHandler bounds the number of entries in a JSON array, and
// optionally the byte length of each string entry. Used for fields like `stop`,
// `stop_token_ids`, and `bad_words` -- vLLM scans every entry against every generated
// token, so unbounded arrays linearly slow inference. MaxEntries=0 disables the array cap,
// MaxEntryLen=0 disables the per-string cap (use 0 for int-only arrays).
type LengthCapListParameterHandler struct {
	MaxEntries  int
	MaxEntryLen int
}

func (h LengthCapListParameterHandler) Apply(ctx *RequestFilterContext, parameter VLLMParameter) error {
	raw, ok := ctx.Document.Array(parameter.Name)
	if !ok {
		return nil
	}
	if h.MaxEntries > 0 && len(raw) > h.MaxEntries {
		return badChatRequest("%s: array length %d exceeds limit %d", parameter.Name, len(raw), h.MaxEntries)
	}
	if h.MaxEntryLen > 0 {
		for i, item := range raw {
			s, ok := item.(string)
			if !ok {
				continue
			}
			if len(s) > h.MaxEntryLen {
				return badChatRequest("%s[%d]: string length %d exceeds limit %d", parameter.Name, i, len(s), h.MaxEntryLen)
			}
		}
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

// DocumentValidator is the contract pure validators in paramvalidators expose.
// Adapter wraps them so their errors become badChatRequest(400) responses without coupling
// each validator to the gateway's pipeline types.
type DocumentValidator interface {
	Validate(map[string]any) error
}

type DocumentValidatorHandler struct {
	Validator DocumentValidator
}

func (h DocumentValidatorHandler) Apply(ctx *RequestFilterContext, _ VLLMParameter) error {
	if h.Validator == nil {
		return nil
	}
	if err := h.Validator.Validate(ctx.Document.Raw()); err != nil {
		return wrapBadChatRequest(err)
	}
	return nil
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

// MaxRequestNestingDepth bounds JSON nesting before we hand the bytes to encoding/json.
// encoding/json allocates O(input size) per recursion level, so a 7 KiB body nested 200
// levels deep blows up to ~180 KiB of map[string]any wrappers. The whitelist rules then
// reject in nanoseconds, but the decoder has already paid the cost. The pre-scan defuses
// that amplification cheaply.
//
// 32 leaves at least 3x headroom over the deepest legitimate request shape we forward:
// `tools[].function.parameters` or `response_format.json_schema.schema` nested under their
// wrappers (~9-10 levels) plus a small allowance for client-side structuring.
const MaxRequestNestingDepth = 32

func decodeChatRequestDocument(body []byte) (*ChatRequestDocument, error) {
	if err := ensureRequestNestingDepth(body, MaxRequestNestingDepth); err != nil {
		return nil, err
	}
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, badChatRequest("parse request: %v", err)
	}
	return &ChatRequestDocument{raw: raw}, nil
}

// ensureRequestNestingDepth performs a byte-level scan that bounds JSON nesting before any
// allocation-heavy decode happens. It tracks quoted strings and escape sequences but
// otherwise ignores semantic structure -- the goal is to bound the decoder, not to validate
// JSON shape; malformed JSON still flows through to the regular parser and gets a normal
// HTTP 400.
func ensureRequestNestingDepth(body []byte, maxDepth int) error {
	depth := 0
	inString := false
	escaped := false
	for _, b := range body {
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch b {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return badChatRequest("request nesting depth exceeds limit %d", maxDepth)
			}
		case '}', ']':
			depth--
			if depth < 0 {
				// More closers than openers. The decoder will reject the malformed body
				// with a normal parse error later; rebase to 0 so subsequent valid blocks
				// are still bounded by maxDepth instead of needing maxDepth+|imbalance|
				// extra opens before tripping the cap.
				depth = 0
			}
		}
	}
	return nil
}

func (d *ChatRequestDocument) Marshal() ([]byte, error) {
	updatedBody, err := json.Marshal(d.raw)
	if err != nil {
		return nil, badChatRequest("marshal request: %v", err)
	}
	return updatedBody, nil
}

func (d *ChatRequestDocument) Has(name string) bool {
	_, ok := d.raw[name]
	return ok
}

// Raw exposes the underlying decoded map for handlers (in this package or external) that
// need direct access to inspect or mutate the top-level fields. The returned map is the live
// document -- modifications are reflected in subsequent reads and the final marshal.
func (d *ChatRequestDocument) Raw() map[string]any {
	return d.raw
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

// DecodeRequest populates ctx.Request from ctx.Document via direct field reads. Previously
// this was a json.Marshal + json.Unmarshal round-trip just to project the document into a
// 5-field struct -- that doubled the allocation count on every request. Direct reads keep
// the behavior (strict types, null-tolerant) but skip the round-trip.
func (ctx *RequestFilterContext) DecodeRequest() error {
	var req chatRequest
	if err := readChatRequestFields(ctx.Document, &req); err != nil {
		return err
	}
	ctx.Request = req
	return nil
}

// SyncRequestView refreshes ctx.Request after PostLimits rules ran. Why we explicitly
// preserve the token fields instead of re-reading them from the document:
//
//   - When the client sends only `max_completion_tokens` (no `max_tokens`),
//     `applyOutputTokenLimits` sets `ctx.Request.MaxTokens` from the resolved
//     `max_completion_tokens` (see request_filters.go:139) but does NOT write a
//     corresponding `max_tokens` key into the document. Re-reading the document would
//     therefore reset `req.MaxTokens` to 0.
//   - In the other three branches of `applyOutputTokenLimits`, the document DOES carry
//     the same value, so preserving from `ctx.Request` is a no-op. Net effect: this
//     branch only matters for the max-completion-only path, locked in by
//     TestNormalizeChatRequestDefaultsAndCapsOutputTokens.
//
// Other fields are re-read so caps applied by PostLimits rules (for example `n` via
// CapUintParameterHandler) propagate into the projection.
func (ctx *RequestFilterContext) SyncRequestView() error {
	var req chatRequest
	if err := readChatRequestFields(ctx.Document, &req); err != nil {
		return err
	}
	req.MaxTokens = ctx.Request.MaxTokens
	req.MaxCompletionTokens = ctx.Request.MaxCompletionTokens
	ctx.Request = req
	return nil
}

func readChatRequestFields(doc *ChatRequestDocument, req *chatRequest) error {
	if raw, ok := doc.Get("model"); ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return badChatRequest("parse request: model must be a string")
		}
		req.Model = s
	}
	if raw, ok := doc.Get("stream"); ok && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return badChatRequest("parse request: stream must be a boolean")
		}
		req.Stream = b
	}
	if err := readUint64Field(doc, "max_tokens", &req.MaxTokens); err != nil {
		return err
	}
	if err := readUint64Field(doc, "max_completion_tokens", &req.MaxCompletionTokens); err != nil {
		return err
	}
	if err := readUint64Field(doc, "n", &req.N); err != nil {
		return err
	}
	return nil
}

func readUint64Field(doc *ChatRequestDocument, name string, dst *uint64) error {
	raw, ok := doc.Get(name)
	if !ok || raw == nil {
		return nil
	}
	n, ok := numericJSONValueAsUint64(raw)
	if !ok {
		return badChatRequest("parse request: %s must be a non-negative integer", name)
	}
	*dst = n
	return nil
}

type VLLMParameterCatalog struct {
	parameters []VLLMParameter
	known      map[string]struct{}
}

var defaultParameterCatalog = defaultVLLMParameterCatalog()

// The catalog is the single source of truth for how each supported OpenAI/vLLM field is treated.
func defaultVLLMParameterCatalog() VLLMParameterCatalog {
	parameters := []VLLMParameter{
		newParameter("model"),
		newParameter("stream"),
		newParameter("messages").
			withRule(RequestFilterStagePreValidation, LengthCapListParameterHandler{MaxEntries: 2048}),
		newParameter("max_tokens"),
		newParameter("max_completion_tokens"),
		newParameter("seed").
			withRule(RequestFilterStagePreValidation, ValidateUintParameterHandler{}),
		newParameter("skip_special_tokens"),
		newParameter("detokenize"),
		newParameter("n").
			withRule(RequestFilterStagePostLimits, CapUintParameterHandler{Max: MaxChatRequestChoices}),
		newParameter("temperature").
			withRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true, Max: floatPointer(MaxTemperature)}),
		newParameter("top_p").
			withRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newParameter("top_k").
			withRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newParameter("min_p").
			withRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true}),
		newParameter("repetition_penalty").
			withRule(RequestFilterStagePostLimits, SanitizeFloatParameterHandler{StripNonFinite: true, Max: floatPointer(MaxRepetitionPenalty)}),
		newParameter("logit_bias").
			withRule(RequestFilterStagePostLimits, SanitizeFloatMapParameterHandler{StripNonFinite: true, Min: floatPointer(-100), Max: floatPointer(100), DropFieldIfEmpty: true, MaxEntries: 1024}),
		newParameter("stop").
			withRule(RequestFilterStagePreValidation, LengthCapListParameterHandler{MaxEntries: 16, MaxEntryLen: 256}),
		newParameter("stop_token_ids").
			withRule(RequestFilterStagePreValidation, LengthCapListParameterHandler{MaxEntries: 64}),
		newParameter("thinking").
			withRule(RequestFilterStagePreValidation, DocumentValidatorHandler{
				Validator: paramvalidators.ThinkingValidator{},
			}),
		newParameter("chat_template_kwargs").
			withRule(RequestFilterStagePreValidation, DocumentValidatorHandler{
				Validator: paramvalidators.ChatTemplateKwargsValidator{
					MaxDepth: 5,
					MaxSize:  16 * 1024,
					MaxNodes: 128,
				},
			}),
		newParameter("tool_choice").
			withRule(RequestFilterStagePreValidation, RejectStringValueParameterHandler{Value: "required", Message: unsupportedChatParameterMessage("tool_choice=required")}),
		newParameter("min_tokens").
			withRule(RequestFilterStagePreValidation, ConditionalStripParameterHandler{
				Predicate: func(ctx *RequestFilterContext) bool {
					return ctx.Document.Has("stop_token_ids")
				},
			}).
			withRule(RequestFilterStagePostLimits, ClampUintToFieldParameterHandler{MaxField: "max_tokens"}),
		newParameter("bad_words").
			withRule(RequestFilterStagePreValidation, SanitizeStringListParameterHandler{
				Keep: func(value string) bool {
					return strings.TrimSpace(value) != ""
				},
				DropFieldIfEmpty: true,
			}).
			withRule(RequestFilterStagePreValidation, LengthCapListParameterHandler{MaxEntries: 64, MaxEntryLen: 128}),
		newParameter("tools").
			withRule(RequestFilterStagePreValidation, CustomParameterHandler{ApplyFunc: stripEmptyToolsAndToolChoice}).
			withRule(RequestFilterStagePreValidation, DocumentValidatorHandler{
				Validator: paramvalidators.ToolsValidator{
					MaxDepth:      5,
					MaxSize:       16 * 1024,
					MaxNodes:      128,
					MaxBranch:     16,
					MaxEnum:       256,
					MaxPatternLen: 512,
				},
			}),
		// OpenAI Chat Completions standard observability fields. No inference-side
		// semantics on the vLLM upstream; clients send them for end-user tracking,
		// distributed tracing, agent control, and streaming token accounting.
		// `user`: type-checked and byte-capped at the gateway boundary so a non-string
		// payload (number, object, …) and an over-long string are caught early instead
		// of being forwarded as a no-op carrier under the 10 MiB body cap.
		newParameter("user").
			withRule(RequestFilterStagePreValidation, DocumentValidatorHandler{
				Validator: paramvalidators.UserValidator{},
			}),
		newParameter("parallel_tool_calls"),
		// metadata: OpenAI bounds it to 16 keys × 64-char keys × 512-char string values;
		// we enforce the same bounds at the gateway boundary as a free defensive cap.
		newParameter("metadata").
			withRule(RequestFilterStagePreValidation, DocumentValidatorHandler{
				Validator: paramvalidators.MetadataValidator{},
			}),
		// stream_options: sub-field whitelist. Only `include_usage` survives;
		// `continuous_usage_stats` is stripped to neutralize vLLM-project/vllm#9028
		// (per-chunk usage counter is wrong under chunked prefill), and any other /
		// future sub-field is default-stripped. If nothing remains the field is dropped
		// so the upstream does not receive an empty `{}` object.
		newParameter("stream_options").
			withRule(RequestFilterStagePreValidation, DocumentValidatorHandler{
				Validator: paramvalidators.StreamOptionsValidator{},
			}),
		newParameter("logprobs").
			withRule(RequestFilterStagePostLimits, ForceLiteralParameterHandler{Value: true}),
		newParameter("top_logprobs").
			withRule(RequestFilterStagePostLimits, ForceLiteralParameterHandler{Value: 5}),
		newParameter("response_format").
			withRule(RequestFilterStagePreValidation, DocumentValidatorHandler{
				Validator: paramvalidators.ResponseFormatValidator{
					MaxDepth:      5,
					MaxSize:       16 * 1024,
					MaxNodes:      128,
					MaxBranch:     16,
					MaxEnum:       256,
					MaxNameLen:    64,
					MaxPatternLen: 512,
				},
			}),
	}
	known := make(map[string]struct{}, len(parameters))
	for _, p := range parameters {
		known[p.Name] = struct{}{}
	}
	return VLLMParameterCatalog{parameters: parameters, known: known}
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
	for key := range ctx.Document.raw {
		if _, ok := c.known[key]; ok {
			continue
		}
		return badChatRequest("%s", unsupportedChatParameterMessage(key))
	}
	return nil
}

func newParameter(name string) VLLMParameter {
	return VLLMParameter{Name: name}
}

func (p VLLMParameter) withRule(stage RequestFilterStage, handler ParameterHandler) VLLMParameter {
	p.Rules = append(p.Rules, ParameterRule{Stage: stage, Handler: handler})
	return p
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
