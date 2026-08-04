package migration

import (
	"fmt"
	"strings"

	"cpa-key-policy/internal/nativeaccess"
	"cpa-key-policy/internal/policy"
)

// Plan converts plugin-issued legacy keys into CPA-native identities plus V2
// access policies. It never mutates either source and never logs plaintext.
func Plan(state *policy.State) ([]string, []nativeaccess.Policy, error) {
	if state == nil {
		return nil, nil, fmt.Errorf("legacy state is required")
	}
	aliases := make(map[string]policy.AliasMapping, len(state.Aliases))
	for _, alias := range state.Aliases {
		aliases[strings.ToLower(strings.TrimSpace(alias.Alias))] = alias
	}
	keys := make([]string, 0, len(state.Keys))
	policies := make([]nativeaccess.Policy, 0, len(state.Keys))
	seenKeys := make(map[string]struct{}, len(state.Keys))
	for _, legacyKey := range state.Keys {
		plain := strings.TrimSpace(legacyKey.PlainKey)
		if plain == "" {
			return nil, nil, fmt.Errorf("legacy key %q has no recoverable plaintext", legacyKey.ID)
		}
		if !policy.MatchHash(plain, legacyKey.KeyHash) {
			return nil, nil, fmt.Errorf("legacy key %q plaintext/hash mismatch", legacyKey.ID)
		}
		hash := nativeaccess.HashKey(plain)
		if _, duplicate := seenKeys[hash]; duplicate {
			return nil, nil, fmt.Errorf("duplicate plaintext identity at legacy key %q", legacyKey.ID)
		}
		seenKeys[hash] = struct{}{}
		if legacyKey.DailyLimitUSD > 0 || legacyKey.WeeklyLimitUSD > 0 {
			return nil, nil, fmt.Errorf("legacy key %q has USD quotas that require explicit V2 conversion", legacyKey.ID)
		}

		grants := make([]nativeaccess.Grant, 0)
		for _, rule := range legacyKey.Models {
			grants = append(grants, grantFromTarget(rule.Alias, policy.AliasTarget{
				Provider: rule.Provider, TargetModel: rule.TargetModel, Group: rule.Group,
			}))
		}
		for _, ref := range legacyKey.Aliases {
			alias, exists := aliases[strings.ToLower(strings.TrimSpace(ref.Alias))]
			if !exists {
				return nil, nil, fmt.Errorf("legacy key %q references missing alias %q", legacyKey.ID, ref.Alias)
			}
			if len(alias.Targets) != 1 {
				return nil, nil, fmt.Errorf("legacy alias %q has %d targets; explicit migration is required", alias.Alias, len(alias.Targets))
			}
			grants = append(grants, grantFromTarget(alias.Alias, alias.Targets[0]))
		}
		if len(grants) == 0 {
			return nil, nil, fmt.Errorf("legacy key %q has no model access", legacyKey.ID)
		}
		keys = append(keys, plain)
		policies = append(policies, nativeaccess.Policy{
			KeyHash: hash,
			Enabled: legacyKey.Enabled,
			Grants:  grants,
			RPM:     legacyKey.RPM,
		})
	}
	return keys, policies, nil
}

func grantFromTarget(_ string, target policy.AliasTarget) nativeaccess.Grant {
	canonical := strings.TrimSpace(target.TargetModel)
	if _, after, found := strings.Cut(canonical, "/"); found {
		canonical = strings.TrimSpace(after)
	}
	return nativeaccess.Grant{
		Provider: target.Provider,
		Model:    canonical,
		Group:    target.Group,
	}
}
