package paramvalidators

import (
	"errors"
	"regexp"
)

// ErrSpecialTokenInContent marks a request that carries a tokenizer special-token literal
// in a field that gets rendered into the chat template prompt. Examples seen in the wild:
//
//   - `<|im_end|>`, `<|im_assistant|>`, `<|im_system|>` — Kimi-K2.6 / Chat-ML role boundaries.
//   - `<|start_header_id|>`, `<|end_header_id|>` — Llama-style header markers.
//   - `[BOS]`, `[EOS]`, `[EOT]`, `[PAD]`, `[UNK]` — SentencePiece/tiktoken sentinels.
//
// When such a literal lands inside `tools[].function.description`, `messages[].content`,
// or any other field that the chat template renders into the prompt string, vLLM's
// tokenizer may encode it as a single special token ID (instead of literal text). The
// attacker then escapes the chat-template structure: prematurely ends a system message,
// impersonates the assistant turn, or splices in a fake system prompt. Whether the
// encoding happens depends on the tokenizer's `allowed_special` setting, which varies
// across vLLM versions and tokenizer flavors — the gateway is the right place to reject
// the literal before it reaches the chat template.
var ErrSpecialTokenInContent = errors.New("content contains a tokenizer special-token literal")

// barDelimitedSpecialTokenPattern matches the `<|name|>` form used by every major LLM
// tokenizer family (Chat-ML, Llama, Qwen, Kimi, Mixtral, GPT-OSS, …). The bracketed-bar
// shape with alphanum + underscore between the bars is reserved across the ecosystem, so
// a strict reject is the right policy — even unknown future names will match.
var barDelimitedSpecialTokenPattern = regexp.MustCompile(`<\|[A-Za-z_][A-Za-z0-9_]*\|>`)

// bracketedSentinelPattern matches the `[NAME]` shape with all-caps letters between the
// brackets. Only names in BracketedSentinels are treated as special tokens; every other
// shape (e.g. `[example]`, `[important]`, citation markers like `[1]`) is left untouched.
var bracketedSentinelPattern = regexp.MustCompile(`\[([A-Z]+)\]`)

// BracketedSentinels is the small allowlist of legacy SentencePiece / BERT-family
// sentinels. Extending it (e.g. for a future `[BOC]` begin-of-context token) is a
// one-line change with no regex surgery.
var BracketedSentinels = map[string]struct{}{
	"BOS":  {},
	"EOS":  {},
	"EOT":  {},
	"PAD":  {},
	"UNK":  {},
	"SEP":  {},
	"CLS":  {},
	"MASK": {},
}

// containsSpecialToken reports whether s contains a tokenizer special-token literal:
// either the bar-delimited `<|name|>` form (any name), or the bracketed `[NAME]` form
// where NAME is in BracketedSentinels.
func containsSpecialToken(s string) bool {
	if barDelimitedSpecialTokenPattern.MatchString(s) {
		return true
	}
	rest := s
	for {
		loc := bracketedSentinelPattern.FindStringSubmatchIndex(rest)
		if loc == nil {
			return false
		}
		name := rest[loc[2]:loc[3]]
		if _, ok := BracketedSentinels[name]; ok {
			return true
		}
		rest = rest[loc[1]:]
	}
}
