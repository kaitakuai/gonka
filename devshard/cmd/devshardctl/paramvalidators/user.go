package paramvalidators

import (
	"errors"
	"fmt"
)

// ErrUserShape covers the wrapper-level rejection: `user` must be a string when present.
// ErrUserLength marks a violation of the byte-length cap.
var (
	ErrUserShape  = errors.New("user: invalid wrapper shape")
	ErrUserLength = errors.New("user: length exceeded")
)

// defaultUserMaxLen is the gateway-side cap on the `user` identifier when no explicit
// limit is configured. OpenAI does not document an upper bound, but production identifiers
// observed in the wild are short (OpenAI's own `user_<random>`, UUIDs, hashed ids,
// email-shaped strings, framework session ids) and stay well under 512 bytes. We pick 512 B
// to fit those use cases comfortably while preventing the field from being used as a 10 MiB
// body-size carrier.
const defaultUserMaxLen = 512

// UserValidator enforces a string type and a byte-length cap on the OpenAI Chat Completions
// `user` field. The field has no inference-side semantics (vLLM ignores it) — clients send
// it for abuse-tracking and observability. Type-checking at the gateway boundary catches
// garbage payloads (`user: 12345`, `user: {...}`) early, and the length cap keeps the field
// from being abused as an unbounded payload carrier despite its no-op upstream behavior.
type UserValidator struct {
	MaxLen int
}

func (v UserValidator) Validate(document map[string]any) error {
	raw, exists := document["user"]
	if !exists {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("%w: must be a string", ErrUserShape)
	}
	maxLen := v.MaxLen
	if maxLen == 0 {
		maxLen = defaultUserMaxLen
	}
	if len(s) > maxLen {
		return fmt.Errorf("%w: %d > %d", ErrUserLength, len(s), maxLen)
	}
	return nil
}
