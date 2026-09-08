package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	modelCatalogSourceOpenAI = "openai-models"
	modelCatalogSourceCodex  = "codex-models"
)

// CatalogGroup is a reusable downstream model catalog assigned to one or more
// key IDs. Key entries support exact IDs, shell-style globs, and "*".
type CatalogGroup struct {
	Name            string   `yaml:"name" json:"name"`
	Keys            []string `yaml:"keys" json:"keys"`
	IncludeUnlisted bool     `yaml:"include_unlisted,omitempty" json:"include_unlisted,omitempty"`
	// IncludeModels and ExcludeModels select source catalog entries by exact
	// model ID or by a single trailing "*" prefix wildcard. Matching is
	// case-insensitive. Exclusions from any matching group win globally.
	IncludeModels []string `yaml:"include_models,omitempty" json:"include_models,omitempty"`
	ExcludeModels []string `yaml:"exclude_models,omitempty" json:"exclude_models,omitempty"`
	// Patch and Remove apply to every source entry retained by IncludeModels or
	// IncludeUnlisted. Explicit Models inherit this patch and may override it
	// with their own model-specific Patch.
	Patch  map[string]any `yaml:"patch,omitempty" json:"patch,omitempty"`
	Remove []string       `yaml:"remove,omitempty" json:"remove,omitempty"`
	Models []CatalogModel `yaml:"models" json:"models"`
}

// CatalogModel clones one source entry from CPA's generated model catalog,
// exposes it under ID, and then applies Patch. Source defaults to ID.
// Remove deletes top-level fields after the patch is merged.
type CatalogModel struct {
	ID     string         `yaml:"id" json:"id"`
	Source string         `yaml:"source,omitempty" json:"source,omitempty"`
	Patch  map[string]any `yaml:"patch,omitempty" json:"patch,omitempty"`
	Remove []string       `yaml:"remove,omitempty" json:"remove,omitempty"`
}

type catalogBulkRule struct {
	IncludeAll    bool
	IncludeModels []string
	Patch         map[string]any
	Remove        []string
}

type catalogSelection struct {
	Models        []CatalogModel
	BulkRules     []catalogBulkRule
	ExcludeModels []string
}

type catalogPolicy struct {
	Groups []CatalogGroup
}

func (a *App) setCatalogPolicy(policy catalogPolicy) {
	if a == nil {
		return
	}
	a.catalogMu.Lock()
	a.catalog = policy
	a.catalogMu.Unlock()
}

func (a *App) catalogPolicySnapshot() catalogPolicy {
	if a == nil {
		return catalogPolicy{}
	}
	a.catalogMu.RLock()
	policy := a.catalog
	a.catalogMu.RUnlock()
	return policy
}

type catalogConfigDocument struct {
	CatalogGroups []CatalogGroup `yaml:"catalog_groups"`
}

func decodeCatalogPolicy(raw []byte) (catalogPolicy, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return catalogPolicy{}, nil
	}
	var doc catalogConfigDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return catalogPolicy{}, err
	}

	seenGroups := make(map[string]struct{}, len(doc.CatalogGroups))
	for gi := range doc.CatalogGroups {
		group := &doc.CatalogGroups[gi]
		group.Name = strings.TrimSpace(group.Name)
		if group.Name == "" {
			return catalogPolicy{}, fmt.Errorf("catalog_groups[%d]: name is required", gi)
		}
		groupKey := strings.ToLower(group.Name)
		if _, exists := seenGroups[groupKey]; exists {
			return catalogPolicy{}, fmt.Errorf("duplicate catalog group %q", group.Name)
		}
		seenGroups[groupKey] = struct{}{}

		keys := make([]string, 0, len(group.Keys))
		seenKeys := make(map[string]struct{}, len(group.Keys))
		for _, rawKey := range group.Keys {
			key := strings.TrimSpace(rawKey)
			if key == "" {
				continue
			}
			if _, err := path.Match(key, "catalog-key-validation"); err != nil {
				return catalogPolicy{}, fmt.Errorf("catalog group %q key pattern %q: %w", group.Name, key, err)
			}
			lk := strings.ToLower(key)
			if _, exists := seenKeys[lk]; exists {
				continue
			}
			seenKeys[lk] = struct{}{}
			keys = append(keys, key)
		}
		group.Keys = keys

		var err error
		group.IncludeModels, err = normalizeCatalogModelPatterns(group.IncludeModels)
		if err != nil {
			return catalogPolicy{}, fmt.Errorf("catalog group %q include_models: %w", group.Name, err)
		}
		group.ExcludeModels, err = normalizeCatalogModelPatterns(group.ExcludeModels)
		if err != nil {
			return catalogPolicy{}, fmt.Errorf("catalog group %q exclude_models: %w", group.Name, err)
		}
		group.Remove = normalizeCatalogRemoveFields(group.Remove)

		seenModels := make(map[string]struct{}, len(group.Models))
		for mi := range group.Models {
			model := &group.Models[mi]
			model.ID = strings.TrimSpace(model.ID)
			model.Source = strings.TrimSpace(model.Source)
			if model.ID == "" {
				return catalogPolicy{}, fmt.Errorf("catalog group %q model %d: id is required", group.Name, mi)
			}
			if model.Source == "" {
				model.Source = model.ID
			}
			lm := strings.ToLower(model.ID)
			if _, exists := seenModels[lm]; exists {
				return catalogPolicy{}, fmt.Errorf("catalog group %q has duplicate model id %q", group.Name, model.ID)
			}
			seenModels[lm] = struct{}{}
			model.Remove = normalizeCatalogRemoveFields(model.Remove)
		}
	}
	return catalogPolicy{Groups: doc.CatalogGroups}, nil
}

func isModelCatalogResponse(req ResponseInterceptRequest) bool {
	switch strings.ToLower(strings.TrimSpace(req.SourceFormat)) {
	case modelCatalogSourceOpenAI, modelCatalogSourceCodex:
		return true
	}
	if req.Metadata != nil {
		if rawPath, ok := req.Metadata["request_path"]; ok && strings.TrimSpace(fmt.Sprint(rawPath)) == "/v1/models" {
			return true
		}
	}
	return false
}

func (a *App) rewriteModelCatalog(req ResponseInterceptRequest) ([]byte, bool) {
	if a == nil {
		return nil, false
	}
	policy := a.catalogPolicySnapshot()
	if len(policy.Groups) == 0 {
		return nil, false
	}

	keyID := catalogKeyID(req.Metadata)
	if keyID == "" {
		// Native CPA keys and other frontend auth providers are not governed by
		// this plugin's catalog groups.
		return nil, false
	}

	selection := catalogSelectionForKey(policy.Groups, keyID)
	body, err := rewriteCatalogBodyWithSelection(req.Body, selection)
	if err != nil {
		// Catalog groups are an allow-list. If a patched host ever changes the
		// response shape unexpectedly, fail closed to an empty valid catalog
		// rather than leaking the global list to a restricted plugin key.
		return emptyCatalogBody(req), true
	}
	return body, !bytes.Equal(body, req.Body)
}

func catalogKeyID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	// The CPA model-list middleware stamps the frontend auth provider. Do not
	// consume another auth plugin's metadata merely because it also has key_id.
	if rawProvider, exists := metadata["access_provider"]; exists {
		provider := strings.TrimSpace(fmt.Sprint(rawProvider))
		if provider != "" && !catalogProviderMatchesPlugin(provider) {
			return ""
		}
	}
	if direct, ok := metadata["key_id"]; ok {
		if key := strings.TrimSpace(fmt.Sprint(direct)); key != "" {
			return key
		}
	}
	raw, ok := metadata["access_metadata"]
	if !ok || raw == nil {
		return ""
	}
	switch meta := raw.(type) {
	case map[string]any:
		if value, ok := meta["key_id"]; ok {
			return strings.TrimSpace(fmt.Sprint(value))
		}
	case map[string]string:
		return strings.TrimSpace(meta["key_id"])
	}
	return ""
}

func catalogProviderMatchesPlugin(provider string) bool {
	provider = strings.TrimSpace(provider)
	if strings.EqualFold(provider, PluginID) {
		return true
	}
	parts := strings.Split(provider, ":")
	return len(parts) == 3 &&
		strings.EqualFold(strings.TrimSpace(parts[0]), "plugin") &&
		strings.EqualFold(strings.TrimSpace(parts[1]), PluginID)
}

func catalogSelectionForKey(groups []CatalogGroup, keyID string) catalogSelection {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return catalogSelection{}
	}
	selection := catalogSelection{}
	seenModels := make(map[string]struct{})
	seenExcludes := make(map[string]struct{})
	for _, group := range groups {
		if !catalogGroupMatchesKey(group, keyID) {
			continue
		}
		for _, pattern := range group.ExcludeModels {
			key := strings.ToLower(pattern)
			if _, exists := seenExcludes[key]; exists {
				continue
			}
			seenExcludes[key] = struct{}{}
			selection.ExcludeModels = append(selection.ExcludeModels, pattern)
		}
		if group.IncludeUnlisted || len(group.IncludeModels) > 0 {
			selection.BulkRules = append(selection.BulkRules, catalogBulkRule{
				IncludeAll:    group.IncludeUnlisted,
				IncludeModels: append([]string(nil), group.IncludeModels...),
				Patch:         cloneJSONMap(group.Patch),
				Remove:        append([]string(nil), group.Remove...),
			})
		}
		for _, model := range group.Models {
			lm := strings.ToLower(model.ID)
			if _, exists := seenModels[lm]; exists {
				continue
			}
			seenModels[lm] = struct{}{}
			model.Patch = mergeCatalogPatches(group.Patch, model.Patch)
			model.Remove = mergeCatalogRemoveFields(group.Remove, model.Remove)
			selection.Models = append(selection.Models, model)
		}
	}
	return selection
}

// catalogModelsForKey keeps the original helper contract for older callers and
// tests. Bulk include patterns are handled by catalogSelectionForKey.
func catalogModelsForKey(groups []CatalogGroup, keyID string) ([]CatalogModel, bool) {
	selection := catalogSelectionForKey(groups, keyID)
	includeUnlisted := false
	for _, rule := range selection.BulkRules {
		if rule.IncludeAll {
			includeUnlisted = true
			break
		}
	}
	return selection.Models, includeUnlisted
}

func catalogGroupMatchesKey(group CatalogGroup, keyID string) bool {
	for _, pattern := range group.Keys {
		matched, err := path.Match(pattern, keyID)
		if err == nil && matched {
			return true
		}
		// Exact key IDs should remain case-insensitive to match policy key lookup.
		if !strings.ContainsAny(pattern, "*?[") && strings.EqualFold(pattern, keyID) {
			return true
		}
	}
	return false
}

func rewriteCatalogBody(body []byte, requested []CatalogModel, includeUnlisted bool) ([]byte, error) {
	selection := catalogSelection{Models: requested}
	if includeUnlisted {
		selection.BulkRules = []catalogBulkRule{{IncludeAll: true}}
	}
	return rewriteCatalogBodyWithSelection(body, selection)
}

func rewriteCatalogBodyWithSelection(body []byte, selection catalogSelection) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	if raw, ok := root["models"]; ok {
		models, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("models catalog field is not an array")
		}
		root["models"] = selectCatalogModelsWithSelection(models, selection, true)
		return json.Marshal(root)
	}
	if raw, ok := root["data"]; ok {
		models, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("data catalog field is not an array")
		}
		root["data"] = selectCatalogModelsWithSelection(models, selection, false)
		return json.Marshal(root)
	}
	return nil, fmt.Errorf("unrecognized model catalog shape")
}

func emptyCatalogBody(req ResponseInterceptRequest) []byte {
	if strings.EqualFold(strings.TrimSpace(req.SourceFormat), modelCatalogSourceCodex) {
		return []byte(`{"models":[]}`)
	}
	if req.Metadata != nil {
		if _, exists := req.Metadata["client_version"]; exists {
			return []byte(`{"models":[]}`)
		}
	}
	return []byte(`{"object":"list","data":[]}`)
}

func selectCatalogModels(source []any, requested []CatalogModel, codex, includeUnlisted bool) []any {
	selection := catalogSelection{Models: requested}
	if includeUnlisted {
		selection.BulkRules = []catalogBulkRule{{IncludeAll: true}}
	}
	return selectCatalogModelsWithSelection(source, selection, codex)
}

func selectCatalogModelsWithSelection(source []any, selection catalogSelection, codex bool) []any {
	byID := make(map[string]map[string]any, len(source))
	ordered := make([]map[string]any, 0, len(source))
	for _, raw := range source {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ids := catalogEntryIDs(entry, codex)
		if len(ids) == 0 {
			continue
		}
		ordered = append(ordered, entry)
		for _, id := range ids {
			if id == "" {
				continue
			}
			key := strings.ToLower(id)
			if _, exists := byID[key]; !exists {
				byID[key] = entry
			}
		}
	}

	out := make([]any, 0, len(selection.Models)+len(ordered))
	seenOutput := make(map[string]struct{})
	replacedSource := make(map[string]struct{})

	// Explicit entries preserve the existing clone/rename behavior and appear
	// first in configuration order.
	for _, spec := range selection.Models {
		if catalogNamesMatchPatterns([]string{spec.ID, spec.Source}, selection.ExcludeModels) {
			continue
		}
		sourceEntry := byID[strings.ToLower(spec.Source)]
		if sourceEntry == nil {
			continue
		}
		sourceIDs := catalogEntryIDs(sourceEntry, codex)
		if catalogNamesMatchPatterns(sourceIDs, selection.ExcludeModels) {
			continue
		}
		outputID := strings.TrimSpace(spec.ID)
		outputKey := strings.ToLower(outputID)
		if _, exists := seenOutput[outputKey]; exists {
			continue
		}
		entry := cloneJSONMap(sourceEntry)
		mergeJSONMap(entry, spec.Patch)
		for _, field := range spec.Remove {
			delete(entry, field)
		}
		setCatalogEntryID(entry, sourceEntry, outputID, codex)
		out = append(out, entry)
		seenOutput[outputKey] = struct{}{}
		for _, id := range sourceIDs {
			replacedSource[strings.ToLower(id)] = struct{}{}
		}
		replacedSource[strings.ToLower(spec.Source)] = struct{}{}
	}

	// Bulk rules keep source IDs unchanged. Every matching rule contributes its
	// field patch in configuration order. Exclusions are global across all
	// catalog groups assigned to the key and therefore always win.
	for _, sourceEntry := range ordered {
		ids := catalogEntryIDs(sourceEntry, codex)
		if catalogNamesMatchPatterns(ids, selection.ExcludeModels) {
			continue
		}
		replaced := false
		for _, id := range ids {
			if _, exists := replacedSource[strings.ToLower(id)]; exists {
				replaced = true
				break
			}
		}
		if replaced {
			continue
		}
		primaryID := catalogEntryPrimaryID(sourceEntry, codex)
		if primaryID == "" {
			continue
		}
		if _, exists := seenOutput[strings.ToLower(primaryID)]; exists {
			continue
		}

		var entry map[string]any
		matched := false
		for _, rule := range selection.BulkRules {
			if !rule.IncludeAll && !catalogNamesMatchPatterns(ids, rule.IncludeModels) {
				continue
			}
			if !matched {
				entry = cloneJSONMap(sourceEntry)
				matched = true
			}
			mergeJSONMap(entry, rule.Patch)
			for _, field := range rule.Remove {
				delete(entry, field)
			}
		}
		if !matched {
			continue
		}
		// Bulk field patches may not rename every matched model to one ID.
		// Restore the source identity after applying arbitrary JSON patches.
		restoreCatalogEntryIdentity(entry, sourceEntry, codex)
		out = append(out, entry)
		for _, id := range ids {
			seenOutput[strings.ToLower(id)] = struct{}{}
		}
	}
	return out
}

func setCatalogEntryID(entry, source map[string]any, id string, codex bool) {
	if codex {
		entry["slug"] = id
		if _, exists := source["id"]; exists {
			entry["id"] = id
		} else {
			delete(entry, "id")
		}
		return
	}
	entry["id"] = id
}

func restoreCatalogEntryIdentity(entry, source map[string]any, codex bool) {
	if codex {
		if value, exists := source["slug"]; exists {
			entry["slug"] = cloneJSONValue(value)
		} else {
			delete(entry, "slug")
		}
		if value, exists := source["id"]; exists {
			entry["id"] = cloneJSONValue(value)
		} else {
			delete(entry, "id")
		}
		return
	}
	if value, exists := source["id"]; exists {
		entry["id"] = cloneJSONValue(value)
	} else {
		delete(entry, "id")
	}
}

func catalogEntryPrimaryID(entry map[string]any, codex bool) string {
	ids := catalogEntryIDs(entry, codex)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func normalizeCatalogModelPatterns(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if strings.Count(value, "*") > 1 || (strings.Contains(value, "*") && !strings.HasSuffix(value, "*")) {
			return nil, fmt.Errorf("model selector %q must be exact or use one trailing *", value)
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func catalogModelPatternMatches(model, pattern string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if model == "" || pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return model == pattern
}

func catalogNamesMatchPatterns(names, patterns []string) bool {
	for _, name := range names {
		for _, pattern := range patterns {
			if catalogModelPatternMatches(name, pattern) {
				return true
			}
		}
	}
	return false
}

func normalizeCatalogRemoveFields(values []string) []string {
	return mergeCatalogRemoveFields(nil, values)
}

func mergeCatalogRemoveFields(base, override []string) []string {
	out := make([]string, 0, len(base)+len(override))
	seen := make(map[string]struct{}, len(base)+len(override))
	for _, values := range [][]string{base, override} {
		for _, raw := range values {
			field := strings.TrimSpace(raw)
			if field == "" {
				continue
			}
			if _, exists := seen[field]; exists {
				continue
			}
			seen[field] = struct{}{}
			out = append(out, field)
		}
	}
	return out
}

func mergeCatalogPatches(base, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := cloneJSONMap(base)
	if out == nil {
		out = make(map[string]any)
	}
	mergeJSONMap(out, override)
	return out
}

func catalogEntryIDs(entry map[string]any, codex bool) []string {
	ids := make([]string, 0, 2)
	if codex {
		if value, ok := entry["slug"].(string); ok {
			ids = append(ids, strings.TrimSpace(value))
		}
	}
	if value, ok := entry["id"].(string); ok {
		ids = append(ids, strings.TrimSpace(value))
	}
	return ids
}

func cloneJSONMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = cloneJSONValue(value)
	}
	return out
}

func cloneJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneJSONMap(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = cloneJSONValue(v[i])
		}
		return out
	default:
		return v
	}
}

func mergeJSONMap(dst map[string]any, patch map[string]any) {
	for key, patchValue := range patch {
		patchMap, patchIsMap := patchValue.(map[string]any)
		if !patchIsMap {
			dst[key] = cloneJSONValue(patchValue)
			continue
		}
		currentMap, currentIsMap := dst[key].(map[string]any)
		if !currentIsMap {
			currentMap = make(map[string]any)
		}
		mergeJSONMap(currentMap, patchMap)
		dst[key] = currentMap
	}
}
