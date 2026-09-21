package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	credKeyRequestBodyOverrides              = "request_body_overrides"
	maxRequestBodyOverrideRules              = 32
	maxRequestBodyOverrideFieldsPerRule      = 64
	maxRequestBodyOverrideModelPatternLength = 200
	maxRequestBodyOverridesJSONBytes         = 32 * 1024
)

// These fields are owned by model mapping, protocol conversion, or session
// isolation. Allowing an account patch to replace them would make routing,
// billing, streaming, or conversation identity inconsistent.
var requestBodyOverrideBlockedFields = map[string]struct{}{
	"model":                {},
	"messages":             {},
	"input":                {},
	"stream":               {},
	"instructions":         {},
	"tools":                {},
	"tool_choice":          {},
	"previous_response_id": {},
	"prompt_cache_key":     {},
}

// IsRequestBodyOverrideEligible reports whether the account can send an
// account-level JSON patch to an OpenAI-compatible HTTP upstream. OAuth and
// setup-token accounts are intentionally excluded because their body also
// carries provider-owned identity/session invariants.
func (a *Account) IsRequestBodyOverrideEligible() bool {
	return a != nil && a.Platform == PlatformOpenAI &&
		(a.Type == AccountTypeAPIKey || a.Type == AccountTypeCPR)
}

func requestBodyOverrideObject(raw any) (map[string]any, bool) {
	switch value := raw.(type) {
	case map[string]any:
		return value, true
	case map[string]map[string]any:
		result := make(map[string]any, len(value))
		for key, patch := range value {
			result[key] = patch
		}
		return result, true
	default:
		return nil, false
	}
}

func requestBodyOverridePatch(raw any) (map[string]any, bool) {
	return requestBodyOverrideObject(raw)
}

func validateRequestBodyOverridePattern(pattern string) error {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return fmt.Errorf("model pattern must not be empty")
	}
	if len(pattern) > maxRequestBodyOverrideModelPatternLength {
		return fmt.Errorf("model pattern %q exceeds %d characters", pattern, maxRequestBodyOverrideModelPatternLength)
	}
	if strings.ContainsAny(pattern, "\r\n\t ") {
		return fmt.Errorf("model pattern %q contains whitespace", pattern)
	}
	wildcardCount := strings.Count(pattern, "*")
	if wildcardCount > 1 || (wildcardCount == 1 && !strings.HasSuffix(pattern, "*")) {
		return fmt.Errorf("model pattern %q may only use one trailing * wildcard", pattern)
	}
	if strings.HasSuffix(pattern, "*") && len(pattern) == 1 {
		return fmt.Errorf("model pattern * is too broad")
	}
	return nil
}

func normalizeRequestBodyOverridePatch(raw any, pattern string) (map[string]any, error) {
	patch, ok := requestBodyOverridePatch(raw)
	if !ok {
		return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
			"request_body_overrides[%q] must be a JSON object", pattern)
	}
	if len(patch) > maxRequestBodyOverrideFieldsPerRule {
		return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
			"request_body_overrides[%q] supports at most %d fields", pattern, maxRequestBodyOverrideFieldsPerRule)
	}

	normalized := make(map[string]any, len(patch))
	for rawField, value := range patch {
		field := strings.TrimSpace(rawField)
		if field == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
				"request_body_overrides field name must not be empty")
		}
		if _, blocked := requestBodyOverrideBlockedFields[field]; blocked {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
				"request_body_overrides field %q cannot be overridden", field)
		}
		if _, duplicate := normalized[field]; duplicate {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
				"duplicate request_body_overrides field %q", field)
		}
		if _, err := json.Marshal(value); err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
				"request_body_overrides field %q is not valid JSON: %v", field, err)
		}
		normalized[field] = value
	}
	return normalized, nil
}

// NormalizeRequestBodyOverrideCredentials validates and canonicalizes the
// per-account model -> top-level JSON patch map. The field lives in the
// credentials JSONB object, so no migration is required.
func NormalizeRequestBodyOverrideCredentials(credentials map[string]any) error {
	if credentials == nil {
		return nil
	}
	raw, exists := credentials[credKeyRequestBodyOverrides]
	if !exists || raw == nil {
		return nil
	}
	entries, ok := requestBodyOverrideObject(raw)
	if !ok {
		return infraerrors.New(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
			"request_body_overrides must be an object of model pattern to JSON object")
	}
	if len(entries) > maxRequestBodyOverrideRules {
		return infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
			"request_body_overrides supports at most %d model rules", maxRequestBodyOverrideRules)
	}

	normalized := make(map[string]any, len(entries))
	for rawPattern, rawPatch := range entries {
		pattern := strings.TrimSpace(rawPattern)
		if err := validateRequestBodyOverridePattern(pattern); err != nil {
			return infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE", "%v", err)
		}
		if _, duplicate := normalized[pattern]; duplicate {
			return infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
				"duplicate request_body_overrides model pattern %q", pattern)
		}
		patch, err := normalizeRequestBodyOverridePatch(rawPatch, pattern)
		if err != nil {
			return err
		}
		normalized[pattern] = patch
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
			"request_body_overrides is not valid JSON: %v", err)
	}
	if len(encoded) > maxRequestBodyOverridesJSONBytes {
		return infraerrors.Newf(http.StatusBadRequest, "INVALID_REQUEST_BODY_OVERRIDE",
			"request_body_overrides exceeds %d bytes", maxRequestBodyOverridesJSONBytes)
	}
	credentials[credKeyRequestBodyOverrides] = normalized
	return nil
}

func resolveRequestBodyOverridePatch(credentials map[string]any, requestedModel string) map[string]any {
	raw, ok := credentials[credKeyRequestBodyOverrides]
	if !ok {
		return nil
	}
	entries, ok := requestBodyOverrideObject(raw)
	if !ok {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	if exact, ok := entries[requestedModel]; ok {
		patch, _ := requestBodyOverridePatch(exact)
		return patch
	}

	type wildcardRule struct {
		pattern string
		patch   map[string]any
	}
	matching := make([]wildcardRule, 0)
	for rawPattern, rawPatch := range entries {
		pattern := strings.TrimSpace(rawPattern)
		if !strings.HasSuffix(pattern, "*") || !strings.HasPrefix(requestedModel, strings.TrimSuffix(pattern, "*")) {
			continue
		}
		patch, ok := requestBodyOverridePatch(rawPatch)
		if !ok {
			continue
		}
		matching = append(matching, wildcardRule{pattern: pattern, patch: patch})
	}
	if len(matching) == 0 {
		return nil
	}
	sort.Slice(matching, func(i, j int) bool {
		if len(matching[i].pattern) != len(matching[j].pattern) {
			return len(matching[i].pattern) > len(matching[j].pattern)
		}
		return matching[i].pattern < matching[j].pattern
	})
	return matching[0].patch
}

// ApplyRequestBodyOverrides returns a new JSON body with a matched patch
// shallow-merged at the top level. Invalid runtime data is ignored so a bad
// legacy JSONB value cannot take an otherwise healthy account offline.
func (a *Account) ApplyRequestBodyOverrides(body []byte, requestedModel string) []byte {
	if a == nil || !a.IsRequestBodyOverrideEligible() || len(body) == 0 || a.Credentials == nil {
		return body
	}
	patch := resolveRequestBodyOverridePatch(a.Credentials, requestedModel)
	if len(patch) == 0 {
		return body
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return body
	}
	for field, value := range patch {
		if _, blocked := requestBodyOverrideBlockedFields[field]; blocked {
			continue
		}
		payload[field] = value
	}
	rebuilt, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return rebuilt
}

const openAIRequestBodyOverrideModelContextKey = "openai_request_body_override_model"

func rememberOpenAIRequestBodyOverrideModel(c *gin.Context, model string) {
	if c == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if existing, ok := c.Get(openAIRequestBodyOverrideModelContextKey); ok && strings.TrimSpace(fmt.Sprint(existing)) != "" {
		return
	}
	c.Set(openAIRequestBodyOverrideModelContextKey, model)
}

func rememberOpenAIRequestBodyOverrideModelFromBody(c *gin.Context, body []byte) {
	rememberOpenAIRequestBodyOverrideModel(c, gjson.GetBytes(body, "model").String())
}

func openAIRequestBodyOverrideModel(c *gin.Context, body []byte) string {
	if c != nil {
		if value, ok := c.Get(openAIRequestBodyOverrideModelContextKey); ok {
			if model := strings.TrimSpace(fmt.Sprint(value)); model != "" {
				return model
			}
		}
	}
	return strings.TrimSpace(gjson.GetBytes(body, "model").String())
}

func applyOpenAIRequestBodyOverrides(c *gin.Context, account *Account, body []byte) []byte {
	if account == nil || !account.IsRequestBodyOverrideEligible() {
		return body
	}
	return account.ApplyRequestBodyOverrides(body, openAIRequestBodyOverrideModel(c, body))
}
