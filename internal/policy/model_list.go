package policy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// claudeCloakedModelPrefix mirrors CPA's internal/client/claude/models prefix.
// CPA rewrites non-"claude-" ids in Anthropic-format model lists as
// prefix + reversed(id); see ResolveClaudeModelIDPrefix in CPA.
const claudeCloakedModelPrefix = "claude-fable-5-dd-"

// ModelListAllowed returns a predicate telling whether the plugin key that
// made the request may use a model id. ok is false when the request does not
// carry an enabled plugin key (built-in CPA keys, disabled plugin), in which
// case the caller must leave the model list untouched.
func (s *Store) ModelListAllowed(headers http.Header) (allowed func(string) bool, ok bool) {
	key, enabled := s.findBySecretWhenEnabled(ExtractAPIKey(headers, nil))
	if !enabled || key == nil || !key.Enabled {
		return nil, false
	}
	set := make(map[string]struct{}, len(key.Models))
	for _, rule := range key.Models {
		if alias := normalizeModelListID(rule.Alias); alias != "" {
			set[alias] = struct{}{}
		}
	}
	return func(id string) bool {
		_, found := set[normalizeModelListID(id)]
		return found
	}, true
}

func normalizeModelListID(id string) string {
	id = strings.TrimSpace(id)
	// Gemini lists use "models/<id>".
	id = strings.TrimPrefix(id, "models/")
	// Anthropic lists may cloak non-Claude ids.
	if strings.HasPrefix(id, claudeCloakedModelPrefix) {
		base, suffix, hasSuffix := splitThinkingSuffix(id)
		if encoded := base[len(claudeCloakedModelPrefix):]; encoded != "" {
			id = reverseString(encoded)
			if hasSuffix {
				id += "(" + suffix + ")"
			}
		}
	}
	return strings.ToLower(id)
}

func splitThinkingSuffix(model string) (base, suffix string, hasSuffix bool) {
	open := strings.LastIndex(model, "(")
	if open == -1 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	return model[:open], model[open+1 : len(model)-1], true
}

func reverseString(value string) string {
	runes := []rune(value)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// FilterModelListBody keeps only allowed entries in a CPA model-list response.
// Supported shapes:
//   - OpenAI and Anthropic: {"data":[{"id":...}], ...}; Anthropic first_id/last_id are refreshed.
//   - Codex client catalog: {"models":[{"slug":...}]}
//   - Gemini: {"models":[{"name":"models/..."}]}
//
// Entries without a recognizable id are kept. It returns changed=false when
// the body is not a recognized list or nothing was removed; callers then keep
// the original body. Entry objects are copied byte-for-byte.
func FilterModelListBody(body []byte, allowed func(string) bool) ([]byte, bool) {
	if allowed == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false
	}
	changed := false
	var keptData []json.RawMessage
	for _, field := range []string{"data", "models"} {
		raw, exists := top[field]
		if !exists {
			continue
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			continue
		}
		kept := make([]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			id, hasID := modelListEntryID(entry)
			if hasID && !allowed(id) {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == len(entries) {
			continue
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			return nil, false
		}
		top[field] = encoded
		if field == "data" {
			keptData = kept
		}
	}
	if !changed {
		return nil, false
	}
	if keptData != nil {
		refreshAnthropicCursor(top, keptData)
	}
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return out, true
}

func modelListEntryID(entry json.RawMessage) (string, bool) {
	var ids struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(entry, &ids); err != nil {
		return "", false
	}
	for _, candidate := range []string{ids.Slug, ids.ID, ids.Name} {
		if strings.TrimSpace(candidate) != "" {
			return candidate, true
		}
	}
	return "", false
}

func refreshAnthropicCursor(top map[string]json.RawMessage, kept []json.RawMessage) {
	_, hasFirst := top["first_id"]
	_, hasLast := top["last_id"]
	if !hasFirst && !hasLast {
		return
	}
	first, last := "", ""
	if len(kept) > 0 {
		first, _ = modelListEntryID(kept[0])
		last, _ = modelListEntryID(kept[len(kept)-1])
	}
	if hasFirst {
		top["first_id"], _ = json.Marshal(first)
	}
	if hasLast {
		top["last_id"], _ = json.Marshal(last)
	}
}
