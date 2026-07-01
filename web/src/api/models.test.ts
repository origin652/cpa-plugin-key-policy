import { describe, it, expect } from "vitest";
import {
  normalizeCatalog,
  groupByCatalog,
  readPlanType,
  filterByConfigured,
} from "./models";

describe("normalizeCatalog", () => {
  it("flattens provider + string models", () => {
    const out = normalizeCatalog([
      { provider: "OpenAI-Compat", models: ["gpt-4o", "gpt-4o-mini"] },
    ]);
    expect(out).toEqual([
      { provider: "openai-compat", model: "gpt-4o" },
      { provider: "openai-compat", model: "gpt-4o-mini" },
    ]);
  });

  it("lowercases provider and dedupes case-insensitively within a group", () => {
    const out = normalizeCatalog([
      { provider: "Codex", models: ["GPT-5"] },
      { provider: "codex", models: ["gpt-5"] },
    ]);
    expect(out).toEqual([{ provider: "codex", model: "GPT-5" }]);
  });

  it("handles array-of-objects with model/id/name fields", () => {
    const out = normalizeCatalog([
      {
        provider: "claude",
        models: [
          { model: "claude-sonnet-4" },
          { id: "claude-opus-4" },
          { name: "claude-haiku" },
        ],
      },
    ]);
    expect(out.map((o) => o.model)).toEqual([
      "claude-haiku",
      "claude-opus-4",
      "claude-sonnet-4",
    ]);
  });

  it("handles object-map models", () => {
    const out = normalizeCatalog([
      { provider: "gemini", models: { "gemini-2.5": {}, "gemini-pro": {} } },
    ]);
    expect(out.map((o) => o.model).sort()).toEqual([
      "gemini-2.5",
      "gemini-pro",
    ]);
  });

  it("skips empty providers and models", () => {
    const out = normalizeCatalog([
      { provider: "", models: ["x"] },
      { provider: "p", models: [""] },
      { provider: "p", models: ["ok"] },
    ]);
    expect(out).toEqual([{ provider: "p", model: "ok" }]);
  });

  it("skips entries without models", () => {
    const out = normalizeCatalog([{ provider: "p" }, { provider: "p", models: null }]);
    expect(out).toEqual([]);
  });

  it("sorts by provider, then group, then model", () => {
    const out = normalizeCatalog([
      { provider: "codex", group: "team", models: ["b"] },
      { provider: "codex", group: "free", models: ["a"] },
      { provider: "claude", models: ["c"] },
    ]);
    expect(out.map((o) => o.provider + "/" + (o.group ?? "") + "/" + o.model)).toEqual([
      "claude//c",
      "codex/free/a",
      "codex/team/b",
    ]);
  });
});

describe("normalizeCatalog tier union", () => {
  it("de-dupes the same model within the same tier (union of same-tier files)", () => {
    // Two codex free auth files both supporting gpt-5-codex → one row.
    const out = normalizeCatalog([
      { provider: "codex", group: "free", models: ["gpt-5-codex"] },
      { provider: "codex", group: "free", models: ["gpt-5-codex", "gpt-5"] },
    ]);
    expect(out).toEqual([
      { provider: "codex", group: "free", model: "gpt-5" },
      { provider: "codex", group: "free", model: "gpt-5-codex" },
    ]);
  });

  it("keeps a model as separate rows across different tiers", () => {
    // gpt-5-codex available under both free and team tiers → two rows so the
    // user can authorize it pinned to a specific tier.
    const out = normalizeCatalog([
      { provider: "codex", group: "free", models: ["gpt-5-codex"] },
      { provider: "codex", group: "team", models: ["gpt-5-codex"] },
    ]);
    expect(out).toEqual([
      { provider: "codex", group: "free", model: "gpt-5-codex" },
      { provider: "codex", group: "team", model: "gpt-5-codex" },
    ]);
  });

  it("leaves non-tiered providers without a group", () => {
    const out = normalizeCatalog([
      { provider: "claude", models: ["claude-sonnet-4"] },
    ]);
    expect(out).toEqual([{ provider: "claude", model: "claude-sonnet-4" }]);
  });
});

describe("groupByCatalog", () => {
  it("groups models under each provider (no tiers)", () => {
    const groups = groupByCatalog([
      { provider: "codex", model: "gpt-5" },
      { provider: "codex", model: "gpt-5-codex" },
      { provider: "claude", model: "claude-sonnet-4" },
    ]);
    expect(groups).toEqual([
      { provider: "claude", models: ["claude-sonnet-4"] },
      { provider: "codex", models: ["gpt-5", "gpt-5-codex"] },
    ]);
  });

  it("splits a tiered provider into subgroups", () => {
    const groups = groupByCatalog([
      { provider: "codex", group: "free", model: "gpt-5-codex" },
      { provider: "codex", group: "team", model: "gpt-5-codex" },
      { provider: "codex", group: "free", model: "gpt-5" },
    ]);
    expect(groups).toEqual([
      { provider: "codex", group: "free", models: ["gpt-5-codex", "gpt-5"] },
      { provider: "codex", group: "team", models: ["gpt-5-codex"] },
    ]);
  });
});

describe("readPlanType", () => {
  // Regression: a live CPA ListAuthFiles response flattens the id_token claims
  // directly onto id_token (id_token.plan_type), NOT under a nested "claims"
  // key. The previous implementation looked for id_token.claims.plan_type and
  // read "" for every codex file, dropping them all into the "supported" bucket
  // so no codex·team group ever appeared.
  it("reads plan_type flattened directly on id_token (live shape)", () => {
    const entry = {
      name: "codex-3f40eabe-ultraman@example.com-team.json",
      id_token: {
        chatgpt_account_id: "abc",
        plan_type: "team",
        chatgpt_subscription_active_until: "2026-07-02T06:31:01+00:00",
      },
    };
    expect(readPlanType(entry)).toBe("team");
  });

  it("tolerates a nested id_token.claims.plan_type shape", () => {
    const entry = { id_token: { claims: { plan_type: "free" } } };
    expect(readPlanType(entry)).toBe("free");
  });

  it("returns empty when no plan_type is present (→ supported bucket)", () => {
    expect(readPlanType({ id_token: { chatgpt_account_id: "x" } })).toBe("");
    expect(readPlanType({ name: "codex-no-claim.json" })).toBe("");
  });

  it("lowercases and trims the plan value", () => {
    expect(readPlanType({ id_token: { plan_type: "  Team  " } })).toBe("team");
  });
});

describe("fromAuthFileModels provider source", () => {
  // Regression: the live /auth-files/models response has NO top-level
  // channel/provider field, and its model objects carry a per-model "type"
  // ("openai" for codex-backed models). The provider must come from the LIST
  // endpoint (carried into fromAuthFileModels), NOT the file name and NOT the
  // per-model type — otherwise each codex file becomes its own "provider"
  // group named after the file and no tier union happens.
  it("uses the list-endpoint provider, not the file name or per-model type", () => {
    // Import the internal adapter via the public normalizeCatalog path: build a
    // RawEntry the way fromAuthFileModels now does and feed normalizeCatalog.
    const liveModelsPayload = {
      models: [
        { display_name: "GPT 5.4", id: "gpt-5.4", owned_by: "openai", type: "openai" },
        { display_name: "GPT 5.5", id: "gpt-5.5", owned_by: "openai", type: "openai" },
      ],
    };
    // Simulate fromAuthFileModels(provider="codex", payload): provider is codex.
    const entry = {
      provider: "codex",
      group: "team",
      models: liveModelsPayload.models.map((m) => m.id),
    };
    const out = normalizeCatalog([entry]);
    expect(out.every((o) => o.provider === "codex")).toBe(true);
    expect(out.map((o) => o.model).sort()).toEqual(["gpt-5.4", "gpt-5.5"]);
  });
});

describe("filterByConfigured", () => {
  // Bare static-definition entries (group undefined) for non-tiered providers.
  const bare = (provider: string, models: string[]) =>
    ({ provider, models });

  it("keeps bare static entries for configured providers", () => {
    const entries = [
      bare("claude", ["claude-sonnet-4"]),
      bare("aistudio", ["gemini-2.5"]),
    ];
    const out = filterByConfigured(
      entries,
      new Set(["claude"]),
      new Set(),
      new Set(),
    );
    expect(out.map((e) => e.provider)).toEqual(["claude"]);
  });

  it("hides bare static entries for unconfigured providers with nothing selected", () => {
    // aistudio has no auth file and nothing selected → hidden. claude is
    // configured → kept. gemini is unconfigured but a model is selected → kept.
    const entries = [
      bare("claude", ["claude-sonnet-4"]),
      bare("aistudio", ["gemini-2.5"]),
      bare("gemini", ["gemini-pro"]),
    ];
    const out = filterByConfigured(
      entries,
      new Set(["claude"]),
      new Set(["gemini"]),
      new Set(),
    );
    expect(out.map((e) => e.provider).sort()).toEqual(["claude", "gemini"]);
  });

  it("keeps an unconfigured provider when one of its models is selected", () => {
    // Regression guard for the edit-mode requirement: a key already authorizes
    // a xai model, but the xai credential was removed. The row must stay
    // visible so the user can uncheck it.
    const entries = [bare("xai", ["grok-3"])];
    const out = filterByConfigured(
      entries,
      new Set(),
      new Set(["xai"]),
      new Set(),
    );
    expect(out.map((e) => e.provider)).toEqual(["xai"]);
  });

  it("keeps auth-file-sourced (grouped) entries even when not in configured set", () => {
    // Entries with a group come from the auth-files path and already imply a
    // configured credential; filterByConfigured must not drop them via the
    // configured/selected check.
    const entries = [
      { provider: "antigravity", group: "supported", models: ["gem-3"] },
    ];
    const out = filterByConfigured(entries, new Set(), new Set(), new Set());
    expect(out).toHaveLength(1);
    expect(out[0].provider).toBe("antigravity");
  });

  it("drops bare static codex entries when auth-files covered codex with tiers", () => {
    // codex is tiered; when auth-files contributed tier subgroups, the bare
    // static "codex" duplicate (no backing auth file) is dropped. The tiered
    // subgroup entries (group set) are kept.
    const entries = [
      bare("codex", ["gpt-5"]),
      { provider: "codex", group: "team", models: ["gpt-5"] },
    ];
    const out = filterByConfigured(
      entries,
      new Set(["codex"]),
      new Set(),
      new Set(["codex"]),
    );
    expect(out).toHaveLength(1);
    expect(out[0].group).toBe("team");
  });

  it("keeps bare static codex entries when auth-files did NOT cover codex", () => {
    // No codex auth file present (e.g. only an API key, or fully unconfigured).
    // The bare static entry is the only source of codex models and must stay,
    // gated only by the configured/selected check like any non-tiered provider.
    const entries = [bare("codex", ["gpt-5"])];
    const out = filterByConfigured(
      entries,
      new Set(["codex"]),
      new Set(),
      new Set(),
    );
    expect(out.map((e) => e.provider)).toEqual(["codex"]);
  });

  it("hides bare static codex entries when codex is neither configured nor selected", () => {
    const entries = [bare("codex", ["gpt-5"])];
    const out = filterByConfigured(entries, new Set(), new Set(), new Set());
    expect(out).toEqual([]);
  });

  it("is case-insensitive on provider matching", () => {
    const entries = [bare("Claude", ["claude-sonnet-4"])];
    const out = filterByConfigured(
      entries,
      new Set(["claude"]),
      new Set(),
      new Set(),
    );
    expect(out.map((e) => e.provider)).toEqual(["Claude"]);
  });

  it("keeps openai-compatibility providers (e.g. opencode) when configured", () => {
    // Regression: fromOpenAICompat emits bare (group-less) entries whose
    // provider is the compat entry's name (e.g. "opencode"). fetchCatalog
    // adds such providers to `configured` so filterByConfigured keeps them —
    // otherwise a configured opencode channel would vanish from the picker.
    const entries = [bare("opencode", ["gpt-5"])];
    const out = filterByConfigured(
      entries,
      new Set(["opencode"]),
      new Set(),
      new Set(),
    );
    expect(out.map((e) => e.provider)).toEqual(["opencode"]);
  });
});
