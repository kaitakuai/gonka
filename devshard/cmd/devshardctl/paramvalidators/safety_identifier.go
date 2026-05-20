package paramvalidators

import (
	"errors"
	"fmt"
)

// ErrSafetyIdentifierShape covers the wrapper-level rejection: `safety_identifier` must be
// a string when present. ErrSafetyIdentifierLength marks a violation of the byte-length cap.
var (
	ErrSafetyIdentifierShape  = errors.New("safety_identifier: invalid wrapper shape")
	ErrSafetyIdentifierLength = errors.New("safety_identifier: length exceeded")
)

// defaultSafetyIdentifierMaxLen mirrors UserValidator's 512 B cap. OpenAI's help-center
// guidance recommends short hashed identifiers (≤64 chars) but the API itself does not
// enforce a hard cap; 512 B is generous enough for hashes/UUIDs/emails while preventing
// the field from being used as a 10 MiB body-size carrier.
const defaultSafetyIdentifierMaxLen = 512

// SafetyIdentifierValidator enforces a string type and a byte-length cap on the OpenAI
// Chat Completions `safety_identifier` field. OpenAI is migrating end-user attribution
// from `user` to `safety_identifier`
// (https://help.openai.com/en/articles/5428082-how-to-incorporate-a-safety-identifier);
// Moonshot mirrors the same field on Kimi K2.6. Validation lives at the gateway boundary;
// per-model gating (only Kimi forwards, others silently strip) is wired in the catalog via
// ModelScopedParameterHandler.
type SafetyIdentifierValidator struct {
	MaxLen int
}

func (v SafetyIdentifierValidator) Validate(vctx ValidatorContext) error {
	raw, exists := vctx.Document["safety_identifier"]
	if !exists {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("%w: must be a string", ErrSafetyIdentifierShape)
	}
	maxLen := v.MaxLen
	if maxLen == 0 {
		maxLen = defaultSafetyIdentifierMaxLen
	}
	if len(s) > maxLen {
		return fmt.Errorf("%w: %d > %d", ErrSafetyIdentifierLength, len(s), maxLen)
	}
	return nil
}
