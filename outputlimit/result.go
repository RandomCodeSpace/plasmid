package outputlimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JSONFallback projects truncated JSON text into the consumer's wire shape.
// BoundJSON still owns the byte accounting and normative truncation report.
type JSONFallback func(text string, report Report) map[string]any

// BoundJSON serializes a result and returns a complete JSON-safe value whose
// final encoding fits both the call grant and policy. The returned byte count
// is the size consumers must charge to the session budget.
func BoundJSON(projected map[string]any, grant int, policy Policy, fallback JSONFallback) (map[string]any, int, error) {
	if fallback == nil {
		return nil, 0, errors.New("bound JSON result: fallback is required")
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil, 0, fmt.Errorf("bound JSON result: %w", err)
	}
	hardLimit := grant
	if policy.MaxBytes > 0 && policy.MaxBytes < hardLimit {
		hardLimit = policy.MaxBytes
	}
	if hardLimit < 2 {
		return nil, 0, errors.New("bound JSON result: output budget is exhausted")
	}
	_, report := applyGrant(string(encoded), hardLimit, policy)
	if !report.Truncated && len(encoded) <= hardLimit {
		return projected, len(encoded), nil
	}
	for sourceLimit := min(hardLimit, len(encoded)); sourceLimit > 0; {
		limited, limitedReport := applyGrant(string(encoded), sourceLimit, policy)
		bounded := fallback(limited, limitedReport)
		final, marshalErr := json.Marshal(bounded)
		if marshalErr != nil {
			return nil, 0, fmt.Errorf("bound truncated JSON result: %w", marshalErr)
		}
		if len(final) <= hardLimit {
			return bounded, len(final), nil
		}
		sourceLimit -= max(1, len(final)-hardLimit)
	}
	minimal := map[string]any{"truncated": true}
	final, err := json.Marshal(minimal)
	if err == nil && len(final) <= hardLimit {
		return minimal, len(final), nil
	}
	return nil, 0, errors.New("bound JSON result: output limit is too small for a truncation marker")
}

func applyGrant(value string, grant int, configured Policy) (string, Report) {
	if grant <= 0 {
		_, original := (Policy{}).Apply(value)
		return "", Report{
			Truncated: true, Reason: ReasonBudget,
			OriginalBytes: original.OriginalBytes, OriginalLines: original.OriginalLines,
		}
	}
	policy := configured
	budgetLimited := policy.MaxBytes <= 0 || grant < policy.MaxBytes
	if policy.MaxBytes <= 0 || policy.MaxBytes > grant {
		policy.MaxBytes = grant
	}
	if policy.MaxLineBytes <= 0 || policy.MaxLineBytes > grant {
		policy.MaxLineBytes = grant
	}
	limited, report := policy.Apply(value)
	if budgetLimited && report.Truncated && (report.Reason == ReasonBytes || report.Reason == ReasonLineLength) {
		oldMarker := Marker(report.Reason, report.KeptBytes, report.OriginalBytes, report.KeptLines, report.OriginalLines)
		report.Reason = ReasonBudget
		limited = strings.Replace(limited, oldMarker, Marker(ReasonBudget, report.KeptBytes, report.OriginalBytes, report.KeptLines, report.OriginalLines), 1)
	}
	return limited, report
}
