import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  parseLiteLLM,
  lookupPrice,
  getPriceTable,
  _resetPriceCache,
} from "./modelPrices";

beforeEach(() => {
  _resetPriceCache();
  sessionStorage.clear();
});

describe("parseLiteLLM", () => {
  it("converts per-token costs to per-million", () => {
    const table = parseLiteLLM({
      "claude-3-5-sonnet-20241022": {
        input_cost_per_token: 0.000003,
        output_cost_per_token: 0.000015,
        cache_read_input_token_cost: 0.0000003,
      },
    });
    const row = lookupPrice(table, "claude-3-5-sonnet-20241022");
    expect(row).not.toBeNull();
    expect(row!.input_price_per_million).toBeCloseTo(3, 5);
    expect(row!.output_price_per_million).toBeCloseTo(15, 5);
    expect(row!.cache_read_price_per_million).toBeCloseTo(0.3, 5);
  });

  it("matches case-insensitively", () => {
    const table = parseLiteLLM({
      "GPT-4o-mini": {
        input_cost_per_token: 0.00000015,
        output_cost_per_token: 0.0000006,
      },
    });
    expect(lookupPrice(table, "gpt-4o-mini")).not.toBeNull();
    expect(lookupPrice(table, "GPT-4O-MINI")).not.toBeNull();
  });

  it("defaults missing cache_read to 0, keeps entry usable", () => {
    const table = parseLiteLLM({
      "ai21.j2-mid-v1": {
        input_cost_per_token: 0.00001,
        output_cost_per_token: 0.000012,
        // no cache_read_input_token_cost
      },
    });
    const row = lookupPrice(table, "ai21.j2-mid-v1");
    expect(row).not.toBeNull();
    expect(row!.input_price_per_million).toBeCloseTo(10, 5);
    expect(row!.output_price_per_million).toBeCloseTo(12, 5);
    expect(row!.cache_read_price_per_million).toBe(0);
  });

  it("skips sample_spec and non-object entries", () => {
    const table = parseLiteLLM({
      sample_spec: { input_cost_per_token: 0.000003, output_cost_per_token: 0.000015 },
      "real-model": { input_cost_per_token: 5, output_cost_per_token: 15 },
      "junk-string": "not-an-object",
      "junk-null": null,
    });
    expect(lookupPrice(table, "sample_spec")).toBeNull();
    expect(lookupPrice(table, "real-model")).not.toBeNull();
    expect(table.size).toBe(1);
  });

  it("skips entries with no usable price info", () => {
    const table = parseLiteLLM({
      "no-price": { mode: "chat", max_input_tokens: 8192 },
      "zero-price": { input_cost_per_token: 0, output_cost_per_token: 0 },
    });
    expect(table.size).toBe(0);
  });

  it("treats negative / non-finite costs as 0", () => {
    const n = Number.NaN;
    const table = parseLiteLLM({
      "weird": {
        input_cost_per_token: -1,
        output_cost_per_token: n,
        cache_read_input_token_cost: Infinity,
      },
    });
    expect(table.size).toBe(0);
  });

  it("returns null for unknown / empty model", () => {
    const table = parseLiteLLM({ "m": { input_cost_per_token: 1 } });
    expect(lookupPrice(table, "nope")).toBeNull();
    expect(lookupPrice(table, "")).toBeNull();
    expect(lookupPrice(null, "m")).toBeNull();
  });
});

describe("getPriceTable caching", () => {
  it("fetches once, caches in sessionStorage, returns from cache on second call", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          "gpt-4o": { input_cost_per_token: 0.0000025, output_cost_per_token: 0.00001 },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const t1 = await getPriceTable();
    expect(t1).not.toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // second call should hit sessionStorage, no new fetch
    const t2 = await getPriceTable();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(lookupPrice(t2, "gpt-4o")).not.toBeNull();

    // cache written to sessionStorage as a stamped envelope
    const raw = sessionStorage.getItem("cpa-key-policy:litellm-prices");
    expect(raw).not.toBeNull();
    const env = JSON.parse(raw!);
    expect(typeof env.fetchedAt).toBe("number");
    expect(Array.isArray(env.table)).toBe(true);
  });

  it("returns null on non-200 without throwing", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 503 })));
    const t = await getPriceTable();
    expect(t).toBeNull();
    // failed fetch must not write a cache
    expect(sessionStorage.getItem("cpa-key-policy:litellm-prices")).toBeNull();
  });

  it("returns null when fetch throws (network)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network")));
    const t = await getPriceTable();
    expect(t).toBeNull();
  });

  it("treats expired cache as a miss and refetches", async () => {
    // seed an expired cache
    const expired = {
      fetchedAt: Date.now() - 25 * 60 * 60 * 1000, // 25h ago
      table: [["stale-model", { input_price_per_million: 0, output_price_per_million: 0, cache_read_price_per_million: 0 }]],
    };
    sessionStorage.setItem("cpa-key-policy:litellm-prices", JSON.stringify(expired));

    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ "fresh": { input_cost_per_token: 1, output_cost_per_token: 2 } }), { status: 200 }),
      ),
    );

    const t = await getPriceTable();
    expect(t).not.toBeNull();
    expect(lookupPrice(t, "fresh")).not.toBeNull();
    expect(lookupPrice(t, "stale-model")).toBeNull();
  });

  it("dedupes concurrent calls to a single fetch (inflight memo)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ "x": { input_cost_per_token: 1 } }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const [a, b] = await Promise.all([getPriceTable(), getPriceTable()]);
    expect(a).not.toBeNull();
    expect(b).not.toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});