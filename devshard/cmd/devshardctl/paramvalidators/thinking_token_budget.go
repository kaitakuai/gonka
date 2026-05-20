package paramvalidators

type ThinkingTokenBudgetDefaultsValidator struct {
	DefaultDivisor uint64
	Models         []string
}

func (v ThinkingTokenBudgetDefaultsValidator) Validate(vctx ValidatorContext) error {
	if !v.matchesModel(vctx.RoutedModel) {
		return nil
	}
	if _, exists := vctx.Document["thinking_token_budget"]; exists {
		return nil
	}
	maxTokens, ok := numericAsUint64(vctx.Document["max_tokens"])
	if !ok || maxTokens == 0 {
		return nil
	}
	value := maxTokens
	if v.DefaultDivisor > 0 {
		value = maxTokens / v.DefaultDivisor
	}
	vctx.Document["thinking_token_budget"] = value
	return nil
}

func (v ThinkingTokenBudgetDefaultsValidator) matchesModel(routedModel string) bool {
	for _, m := range v.Models {
		if m == routedModel {
			return true
		}
	}
	return false
}
