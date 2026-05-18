package paramvalidators

import "testing"

func TestContainsSpecialToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Bar-delimited (matches)
		{name: "im_end", in: "before <|im_end|> after", want: true},
		{name: "im_user", in: "<|im_user|>", want: true},
		{name: "im_assistant", in: "<|im_assistant|>", want: true},
		{name: "im_system", in: "<|im_system|>", want: true},
		{name: "im_middle", in: "<|im_middle|>", want: true},
		{name: "start_header_id", in: "<|start_header_id|>", want: true},
		{name: "end_header_id", in: "<|end_header_id|>", want: true},
		{name: "endoftext", in: "<|endoftext|>", want: true},
		{name: "vision_start (Qwen)", in: "<|vision_start|>", want: true},
		{name: "fim_prefix", in: "<|fim_prefix|>", want: true},
		{name: "unknown future token", in: "<|future_unknown_token|>", want: true},

		// Bracketed allowlist (matches)
		{name: "BOS", in: "[BOS]", want: true},
		{name: "EOS", in: "[EOS]", want: true},
		{name: "EOT", in: "[EOT]", want: true},
		{name: "PAD", in: "[PAD]", want: true},
		{name: "UNK", in: "[UNK]", want: true},
		{name: "SEP", in: "[SEP]", want: true},
		{name: "CLS", in: "[CLS]", want: true},
		{name: "MASK", in: "[MASK]", want: true},
		// Bracketed allowlist embedded in larger text (matches; short-circuit must traverse)
		{name: "BOS prefixed", in: "noise [BOS] more noise", want: true},
		{name: "BOS after non-allowlisted bracket", in: "[FOO] [BOS]", want: true},

		// Legitimate text (no match — false-positive check)
		{name: "plain prose", in: "Get the current temperature for a city", want: false},
		{name: "citation marker", in: "as shown in [1]", want: false},
		{name: "bracketed example", in: "use [example] here", want: false},
		{name: "bracketed important", in: "[important]", want: false},
		{name: "bracketed lowercase keyword", in: "[bos]", want: false}, // strict-case allowlist
		{name: "bracketed non-allowlisted upper", in: "[FOO]", want: false},
		{name: "bracketed mixed-case", in: "[Bos]", want: false},
		{name: "bracketed with digits", in: "[BOS1]", want: false},
		{name: "single bar pair", in: "<|>", want: false},
		{name: "empty bars", in: "<||>", want: false},
		{name: "bars with space inside", in: "<| name |>", want: false},
		{name: "bars with hyphen inside", in: "<|im-end|>", want: false}, // ascii hyphen not in [A-Za-z0-9_]
		{name: "empty string", in: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsSpecialToken(tt.in); got != tt.want {
				t.Errorf("containsSpecialToken(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
