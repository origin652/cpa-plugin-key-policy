// Shapes mirrored from the cpa-key-policy plugin (internal/policy/config.go)
// and CPA management responses. Only the fields the UI needs are declared.

export interface ModelRule {
  alias: string;
  provider: string;
  target_model: string;
  // Optional tier/plan narrowing for providers whose auth files carry an
  // identity claim (codex plan_type, antigravity tier). Empty = "any file for
  // the provider" (legacy). The plugin's Scheduler filters auth candidates by
  // this so a downstream key pinned to, say, codex "team" only ever lands on a
  // team auth file. UI catalog groups mirror this value.
  group?: string;
  input_price_per_million?: number;
  output_price_per_million?: number;
  cache_read_price_per_million?: number;
}

export interface UsageSummary {
  daily_usd: number;
  weekly_usd: number;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  daily_reset_at?: string;
  weekly_reset_at?: string;
  // Cache reporting (omitted when zero). Hit-rate is derived client-side as
  // cache_read_tokens / (cache_read_tokens + input_tokens).
  daily_cache_cost_usd?: number;
  weekly_cache_cost_usd?: number;
  daily_cache_read_tokens?: number;
  weekly_cache_read_tokens?: number;
  daily_input_tokens?: number;
  weekly_input_tokens?: number;
}

export interface KeyPublic {
  id: string;
  name: string;
  enabled: boolean;
  key_preview: string;
  rpm: number;
  models: ModelRule[];
  daily_limit_usd: number;
  weekly_limit_usd: number;
  usage: UsageSummary;
  created_at?: string;
  updated_at?: string;
}

export interface KeyWriteRequest {
  id: string;
  name?: string;
  enabled?: boolean;
  key?: string;
  rpm?: number;
  models?: ModelRule[];
  daily_limit_usd?: number;
  weekly_limit_usd?: number;
}

export interface CreateKeyResponse {
  key: KeyPublic;
  plain_key: string;
  generated: boolean;
}

export interface RotateKeyResponse {
  key: KeyPublic;
  plain_key: string;
  generated: boolean;
}

// A model the user can pick when creating/editing a key.
export interface CatalogModel {
  provider: string;
  // group is set for providers whose auth files carry a tier/plan identity
  // (codex plan_type, antigravity tier). Same model may appear under several
  // groups when multiple tiers' auth files all support it — each is a distinct
  // selectable row pinning a different tier.
  group?: string;
  model: string;
}

export interface StatusResponse {
  enabled: boolean;
  state_file: string;
  key_count: number;
  rpm_usage?: Record<string, unknown>;
}
