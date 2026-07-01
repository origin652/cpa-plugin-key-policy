import { describe, it, expect } from "vitest";
import { normalizeCatalog, groupByCatalog, readPlanType } from "./models";

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
