package policy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// claudeCloakedModelPrefix mirrors CPA's internal/client/claude/models prefix.
// CPA rewrites non-"claude-" ids in Anthropic-format model lists as
// prefix + reversed(id); see ResolveClaudeModelIDPrefix in CPA.
const claudeCloakedModelPrefix = "claude-fable-5-dd-"

// ModelListPolicy narrows a model list to the models one plugin key may call.
// It keeps the key's aliases and, for aliases that CPA does not list under
// that name, appends an entry derived from the alias target so custom alias
// names stay selectable.
type ModelListPolicy struct {
	// rules maps normalized alias -> first rule for that alias.
	rules map[string]ModelRule
}

// ModelListPolicy resolves the policy for the plugin key that made the
// request. ok is false when the request does not carry an enabled plugin key
// (built-in CPA keys, disabled plugin): the caller must then leave the model
// list untouched.
func (s *Store) ModelListPolicy(headers http.Header) (*ModelListPolicy, bool) {
	key, enabled := s.findBySecretWhenEnabled(ExtractAPIKey(headers, nil))
	if !enabled || key == nil || !key.Enabled {
		return nil, false
	}
	rules := make(map[string]ModelRule, len(key.Models))
	for _, rule := range key.Models {
		alias := normalizeModelListID(rule.Alias)
		if alias == "" {
			continue
		}
		if _, exists := rules[alias]; !exists {
			rules[alias] = rule
		}
	}
	return &ModelListPolicy{rules: rules}, true
}

// Allowed reports whether the key may call the given model id.
func (p *ModelListPolicy) Allowed(id string) bool {
	if p == nil {
		return true
	}
	_, found := p.rules[normalizeModelListID(id)]
	return found
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

func cloakModelListID(id string) string {
	if id == "" || strings.HasPrefix(id, "claude-") {
		return id
	}
	return claudeCloakedModelPrefix + reverseString(id)
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

type modelListFormat int

const (
	formatOpenAI modelListFormat = iota
	formatCodex
	formatAnthropic
	formatGemini
)

// FilterModelListBody keeps only the models the policy allows in a CPA
// model-list response, and appends entries for aliases that the upstream
// catalog does not list under their alias name.
//
// Supported shapes:
//   - OpenAI and Anthropic: {"data":[{"id":...}], ...}; Anthropic first_id/last_id are refreshed.
//   - Codex client catalog: {"models":[{"slug":...}]}
//   - Gemini: {"models":[{"name":"models/..."}]}
//
// Entries without a recognizable id are kept. It returns changed=false when
// the body is not a recognized list or nothing changed; callers then keep the
// original body.
func FilterModelListBody(body []byte, policy *ModelListPolicy) ([]byte, bool) {
	if policy == nil || len(bytes.TrimSpace(body)) == 0 {
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
		format := detectModelListFormat(field, top, entries)
		byID := make(map[string]json.RawMessage, len(entries))
		kept := make([]json.RawMessage, 0, len(entries))
		present := make(map[string]bool, len(entries))
		fieldChanged := false
		for _, entry := range entries {
			id, hasID := modelListEntryID(entry)
			normalized := ""
			if hasID {
				normalized = normalizeModelListID(id)
				if _, exists := byID[normalized]; !exists {
					byID[normalized] = entry
				}
			}
			if hasID && !policy.Allowed(id) {
				fieldChanged = true
				continue
			}
			if normalized != "" {
				present[normalized] = true
			}
			kept = append(kept, entry)
		}
		if added, ok := policy.synthesize(format, present, byID); ok {
			kept = append(kept, added...)
			fieldChanged = true
		}
		if !fieldChanged {
			continue
		}
		changed = true
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

// synthesize builds entries for allowed aliases missing from the list.
// present holds the normalized ids of entries that survived filtering.
func (p *ModelListPolicy) synthesize(format modelListFormat, present map[string]bool, byID map[string]json.RawMessage) ([]json.RawMessage, bool) {
	missing := make([]string, 0, len(p.rules))
	for alias := range p.rules {
		if !present[alias] {
			missing = append(missing, alias)
		}
	}
	if len(missing) == 0 {
		return nil, false
	}
	sort.Strings(missing)
	added := make([]json.RawMessage, 0, len(missing))
	for _, alias := range missing {
		rule := p.rules[alias]
		var base json.RawMessage
		if target := normalizeModelListID(rule.TargetModel); target != "" {
			base = byID[target]
		}
		entry, ok := rewriteModelListEntry(base, format, alias)
		if !ok {
			continue
		}
		added = append(added, entry)
	}
	if len(added) == 0 {
		return nil, false
	}
	return added, true
}

// rewriteModelListEntry clones an existing catalog entry under a new id so the
// client keeps the model metadata. Without a base entry it builds a minimal one.
func rewriteModelListEntry(base json.RawMessage, format modelListFormat, alias string) (json.RawMessage, bool) {
	if alias == "" {
		return nil, false
	}
	if len(base) == 0 && format == formatCodex {
		// The Codex catalog has required metadata fields; a hand-built entry
		// makes Codex reject the whole catalog. Only aliases whose target the
		// catalog describes can be shown there.
		return nil, false
	}
	entry := map[string]json.RawMessage{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &entry); err != nil {
			entry = map[string]json.RawMessage{}
		}
	}
	set := func(key, value string) {
		encoded, err := json.Marshal(value)
		if err == nil {
			entry[key] = encoded
		}
	}
	switch format {
	case formatCodex:
		set("slug", alias)
	case formatAnthropic:
		set("id", cloakModelListID(alias))
		if _, exists := entry["type"]; !exists {
			set("type", "model")
		}
	case formatGemini:
		set("name", "models/"+alias)
		if len(base) == 0 {
			set("displayName", alias)
		}
	default:
		set("id", alias)
		if _, exists := entry["object"]; !exists {
			set("object", "model")
		}
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func detectModelListFormat(field string, top map[string]json.RawMessage, entries []json.RawMessage) modelListFormat {
	if field == "data" {
		_, hasFirst := top["first_id"]
		_, hasMore := top["has_more"]
		if hasFirst || hasMore {
			return formatAnthropic
		}
		return formatOpenAI
	}
	for _, entry := range entries {
		var probe struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(entry, &probe); err != nil {
			continue
		}
		if probe.Slug != "" {
			return formatCodex
		}
		if probe.Name != "" {
			return formatGemini
		}
	}
	return formatOpenAI
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
