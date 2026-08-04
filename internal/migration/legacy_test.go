package migration

import (
	"testing"

	"cpa-key-policy/internal/policy"
)

func TestPlanPreservesKeyAndConvertsAliasToCanonicalGrant(t *testing.T) {
	const plain = "sk-existing"
	hash, err := policy.HashKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	state := &policy.State{
		Keys: []policy.KeyConfig{{
			ID:       "hao",
			PlainKey: plain,
			KeyHash:  hash,
			Enabled:  true,
			RPM:      20,
			Aliases:  []policy.KeyAliasRef{{Alias: "codex-csil-gpt-5.6-sol"}},
		}},
		Aliases: []policy.AliasMapping{{
			Alias: "codex-csil-gpt-5.6-sol",
			Targets: []policy.AliasTarget{{
				Provider: "codex", TargetModel: "codex-csil/gpt-5.6-sol", Group: "classify:csil",
			}},
		}},
	}
	keys, policies, err := Plan(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != plain || len(policies) != 1 {
		t.Fatalf("migration lost identity: keys=%d policies=%d", len(keys), len(policies))
	}
	grant := policies[0].Grants[0]
	if grant.Model != "gpt-5.6-sol" || grant.Group != "classify:csil" ||
		grant.UpstreamPrefix != "" || len(grant.AcceptedModels) != 0 {
		t.Fatalf("unexpected grant: %#v", grant)
	}
}

func TestPlanFailsClosedOnAmbiguousAlias(t *testing.T) {
	const plain = "sk-existing"
	hash, _ := policy.HashKey(plain)
	_, _, err := Plan(&policy.State{
		Keys: []policy.KeyConfig{{
			ID: "ambiguous", PlainKey: plain, KeyHash: hash, Enabled: true,
			Aliases: []policy.KeyAliasRef{{Alias: "multi"}},
		}},
		Aliases: []policy.AliasMapping{{
			Alias: "multi",
			Targets: []policy.AliasTarget{
				{Provider: "a", TargetModel: "m"},
				{Provider: "b", TargetModel: "m"},
			},
		}},
	})
	if err == nil {
		t.Fatal("multi-target legacy alias must require explicit migration")
	}
}
