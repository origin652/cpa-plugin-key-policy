import { apiClient } from "./client";
import type { CatalogModel } from "../types";

// CPA has no single "list providers+models" endpoint. We compose from several
// management routes. The raw shapes are loose, so each adapter pulls strings out
// defensively and feeds them into normalizeCatalog, which is the unit-tested core.

const STATIC_CHANNELS = [
  "claude",
  "gemini",
  "vertex",
  "aistudio",
  "codex",
  "kimi",
  "antigravity",
  "xai",
] as const;

// Providers whose auth files carry a tier/plan identity claim. Only these get
// tier subgroups in the picker (codex via id_token plan_type, antigravity via
// tier_id). Everything else stays a flat per-provider group — there's no
// meaningful "tier" to split on, and the plugin Scheduler won't filter them.
const TIERED_PROVIDERS = new Set(["codex", "antigravity"]);

// "supported" is the synthetic group for a tiered-provider auth file whose
// identity claim we couldn't read (e.g. an old codex file with no id_token).
// It must NOT be confused with a real tier: a key pinned to "team" never lands
// on a "supported" file, and vice versa. The plugin Scheduler treats
// "supported"/"unknown" as the untiered bucket.
const SUPPORTED_GROUP = "supported";

// Map the per-channel API-key management endpoints to the provider identity
// used in the catalog. The endpoint returns its key list under a top-level
// key named like the channel (e.g. { "gemini-api-key": [...] }); the provider
// group in the picker is the bare name ("gemini"). A non-empty list means the
// user has configured at least one API-key credential for that provider.
const API_KEY_CHANNELS: Record<string, string> = {
  "gemini-api-key": "gemini",
  "claude-api-key": "claude",
  "codex-api-key": "codex",
  "vertex-api-key": "vertex",
};

interface RawEntry {
  provider?: string;
  models?: unknown;
  // group is only meaningful when populated from an auth-file source; static
  // channels and openai-compat entries have no tier concept and leave it empty.
  group?: string;
}

// Collect (provider, [group], model) tuples from heterogeneous CPA responses.
// Within a single (provider, group) bucket, duplicate models are de-duplicated
// — same model supported by multiple same-tier auth files appears once. A model
// supported by BOTH "free" and "team" tiers appears as two separate rows so the
// user can authorize it under a specific tier (real isolation via Scheduler).
export function normalizeCatalog(entries: RawEntry[]): CatalogModel[] {
  const seen = new Set<string>();
  const out: CatalogModel[] = [];
  for (const e of entries) {
    const provider = (e.provider ?? "").toString().trim().toLowerCase();
    if (!provider) continue;
    const group = (e.group ?? "").toString().trim().toLowerCase();
    for (const m of toStrings(e.models)) {
      const model = m.trim();
      if (!model) continue;
      const key = provider + "" + group + "" + model.toLowerCase();
      if (seen.has(key)) continue;
      seen.add(key);
      const row: CatalogModel = { provider, model };
      if (group) row.group = group;
      out.push(row);
    }
  }
  // Stable sort: provider, then group (empty group sorts first within provider),
  // then model (case-insensitive).
  out.sort((a, b) => {
    if (a.provider !== b.provider) return a.provider.localeCompare(b.provider);
    const ga = a.group ?? "";
    const gb = b.group ?? "";
    if (ga !== gb) return ga.localeCompare(gb);
    return a.model.toLowerCase().localeCompare(b.model.toLowerCase());
  });
  return out;
}

function toStrings(v: unknown): string[] {
  if (v == null) return [];
  if (typeof v === "string") return [v];
  if (Array.isArray(v)) {
    return v
      .map((x) => {
        if (typeof x === "string") return x;
        if (x && typeof x === "object") {
          const mo = (x as Record<string, unknown>).model;
          if (typeof mo === "string") return mo;
          const id = (x as Record<string, unknown>).id;
          if (typeof id === "string") return id;
          const name = (x as Record<string, unknown>).name;
          if (typeof name === "string") return name;
        }
        return "";
      })
      .filter((s) => s !== "");
  }
  if (typeof v === "object") {
    // object map like { "model-a": {...}, "model-b": {...} }
    return Object.keys(v as Record<string, unknown>);
  }
  return [];
}

// --- CPA response adapters (best-effort, defensive) ---

function fromOpenAICompat(payload: unknown): RawEntry[] {
  const root = payload as Record<string, unknown> | null;
  const list = root?.["openai-compatibility"];
  if (!Array.isArray(list)) return [];
  return list.map((item) => {
    const o = item as Record<string, unknown> | null;
    // Prefer `name` (e.g. "opencode") as the provider identity on CPA's
    // openai-compatibility entries, then fall back to provider/id.
    const provider =
      (o?.["name"] as string) ??
      (o?.["provider"] as string) ??
      (o?.["id"] as string) ??
      "openai-compat";
    return { provider, models: o?.["models"] };
  });
}

// One auth-file row from /v0/management/auth-files. We only need the file name
// (to join against per-file models) and the tier identity claim (codex
// id_token plan_type, antigravity tier). The ListAuthFiles response exposes
// plan_type under id_token.claims.plan_type; the flat "account_type" sibling is
// a fallback. Everything else is ignored.
interface AuthFileMeta {
  name: string;
  // provider as reported by the auth-files LIST endpoint (e.g. "codex",
  // "antigravity", "claude"). The per-file /auth-files/models endpoint does NOT
  // echo a provider/channel — its models objects carry a per-model "type"
  // ("openai" for codex-backed models) which is NOT the auth provider. We must
  // carry the list-endpoint provider here so each file's models land under the
  // right provider group; otherwise the file name leaks in as the "provider".
  provider: string;
  planType: string;
}

function fromAuthFiles(payload: unknown): AuthFileMeta[] {
  const root = payload as Record<string, unknown> | null;
  const list = root?.["auth-files"] ?? root?.["files"];
  if (!Array.isArray(list)) return [];
  const out: AuthFileMeta[] = [];
  for (const item of list) {
    const o = (item ?? {}) as Record<string, unknown>;
    const name = ((o["name"] as string) ?? (o["id"] as string) ?? "").trim();
    if (!name) continue;
    const provider = ((o["provider"] as string) ?? (o["type"] as string) ?? "").trim().toLowerCase();
    const planType = readPlanType(o);
    out.push({ name, provider, planType });
  }
  return out;
}

// Extract the tier/plan identity from an auth-files list entry. codex's
// ListAuthFiles response flattens the id_token claims directly onto the
// id_token object (id_token.plan_type), NOT under a nested "claims" key —
// verified against a live CPA build. We still tolerate the nested shape
// (id_token.claims.plan_type) defensively in case a future build restructures.
// antigravity's tier identity isn't exposed on the list entry on current
// builds; its files fall through to the "supported" bucket (the Scheduler
// side reads Attributes["tier"] for antigravity, which is a separate path).
// Returns "" when no recognizable claim is present (→ "supported" bucket).
// Exported for unit testing against real ListAuthFiles payloads.
export function readPlanType(entry: Record<string, unknown>): string {
  const idToken = entry["id_token"];
  if (idToken && typeof idToken === "object") {
    const tok = idToken as Record<string, unknown>;
    // Primary path (verified live): plan_type flattened directly on id_token.
    const plan = tok["plan_type"];
    if (typeof plan === "string" && plan.trim() !== "") {
      return plan.trim().toLowerCase();
    }
    // Defensive fallback: nested under a "claims" sub-object.
    const claims = tok["claims"];
    if (claims && typeof claims === "object") {
      const nested = (claims as Record<string, unknown>)["plan_type"];
      if (typeof nested === "string" && nested.trim() !== "") {
        return nested.trim().toLowerCase();
      }
    }
  }
  // antigravity tier identity, when present, sits at the top level.
  const tier = entry["tier"];
  if (typeof tier === "string" && tier.trim() !== "") {
    return tier.trim().toLowerCase();
  }
  return "";
}

// Build a RawEntry from a per-file /auth-files/models response. The provider
// comes from the LIST endpoint (carried in `provider`), NOT the models
// payload — the models objects report a per-model "type" ("openai" for codex
// backed models) which is the upstream format, not the auth provider, and the
// response itself has no top-level channel/provider field.
function fromAuthFileModels(provider: string, payload: unknown): RawEntry[] {
  const root = payload as Record<string, unknown> | null;
  const models = root?.["models"] ?? root?.["available_models"];
  return [{ provider, models }];
}

function fromModelDefinitions(channel: string, payload: unknown): RawEntry[] {
  const root = payload as Record<string, unknown> | null;
  const models = root?.["models"] ?? root?.["definitions"];
  return [{ provider: channel, models }];
}

// Is the *-api-key list at `endpoint` non-empty? CPA's Get<Key> handlers return
// 200 with a top-level "<channel>-api-key": [...] array (nil/empty when none
// configured), NOT a 404, so we must inspect the body to know whether a
// credential is actually present. Returns the mapped provider name when at
// least one key exists, else "".
function apiKeyProviderIfConfigured(endpoint: string, payload: unknown): string {
  const provider = API_KEY_CHANNELS[endpoint];
  if (!provider) return "";
  const root = payload as Record<string, unknown> | null;
  if (!root) return "";
  const list = root[endpoint];
  return Array.isArray(list) && list.length > 0 ? provider : "";
}

// Filter the collected raw entries down to those that should be visible in the
// picker. Bare (group-less) static entries — from model-definitions/<channel> —
// are dropped when EITHER:
//   1. They're a tiered provider (codex, antigravity) that the auth-files pass
//      already contributed tier subgroups for, so the bare row is a duplicate
//      with no backing auth file (the "codex · team" subgroup is the real one).
//   2. Their provider is neither configured (no credential) nor has a
//      currently-selected model — i.e. an unconfigured channel that should be
//      hidden from the picker.
// Entries carrying a `group` are auth-file sourced and already imply a
// configured credential, so they're kept unconditionally.
// Exported for unit testing.
export function filterByConfigured(
  entries: RawEntry[],
  configured: Set<string>,
  selected: Set<string>,
  tieredFromAuth: Set<string>,
): RawEntry[] {
  const out: RawEntry[] = [];
  for (const e of entries) {
    const provider = (e.provider ?? "").toLowerCase();
    if (e.group === undefined) {
      // Bare static entry (from model-definitions/<channel>).
      // Drop when it's a tiered provider already covered by auth-files (the
      // tier subgroups are the real, backed rows; this bare one is a dup with
      // no auth file behind it)...
      const dupOfTiered =
        TIERED_PROVIDERS.has(provider) && tieredFromAuth.has(provider);
      // ...or when the provider is neither configured nor has a selected model
      // (unconfigured channel should be hidden, but an edited key's rows stay
      // visible so the user can uncheck them).
      const unconfigured =
        !configured.has(provider) && !selected.has(provider);
      if (dupOfTiered || unconfigured) continue;
    }
    out.push(e);
  }
  return out;
}

// Fetch the composed catalog. Failures of individual sources are swallowed so
// that one unavailable endpoint doesn't blank the whole picker. A 401/403 here
// is real (bad key) and surfaces through the shared client as a forced
// re-login — that's intended; we don't mask auth failures.
//
// `selectedProviders` is the set of providers (lowercased) the caller already
// has model rules for (edit-mode prefill). Providers in this set stay visible
// even when unconfigured, so the user can see and uncheck their rows. New-key
// mode passes nothing (empty set) and only configured channels appear.
export async function fetchCatalog(
  selectedProviders?: Set<string>,
): Promise<CatalogModel[]> {
  const c = apiClient();
  const entries: RawEntry[] = [];
  // Tiered providers that the auth-files path contributed models for. Filled
  // during the auth-files pass; read in filterByConfigured to suppress bare
  // static entries for providers already covered with tier subgroups.
  const authFileTieredProviders = new Set<string>();
  // Providers the user has actually configured a credential for (an OAuth auth
  // file is present, or a non-empty *-api-key list). Bare static-definition
  // entries for providers NOT in this set are hidden unless a model under them
  // is already selected (see filterByConfigured).
  const configuredProviders = new Set<string>();
  const selected = new Set<string>();
  for (const p of selectedProviders ?? []) selected.add(p.toLowerCase());

  const safe = async <T>(p: Promise<{ data: T }>, apply: (d: T) => void) => {
    try {
      const { data } = await p;
      apply(data);
    } catch {
      /* skip unavailable source */
    }
  };

  await safe(
    c.get("/v0/management/openai-compatibility"),
    (d) => {
      const compatEntries = fromOpenAICompat(d);
      // Every provider listed under openai-compatibility is, by construction,
      // one the user configured a credential for (the list only contains
      // configured compat entries). Record that so filterByConfigured doesn't
      // drop their bare entries as "unconfigured".
      for (const e of compatEntries) {
        const p = (e.provider ?? "").toLowerCase();
        if (p) configuredProviders.add(p);
      }
      entries.push(...compatEntries);
    },
  );

  // Per-channel API-key endpoints. These responses carry their key list under a
  // top-level "<channel>-api-key" array (NOT under "models"/"keys"), so
  // fromChannelKey yields no models here — we use them solely to detect whether
  // the user has configured an API-key credential, marking the mapped provider
  // as configured when the list is non-empty.
  for (const ch of Object.keys(API_KEY_CHANNELS)) {
    await safe(c.get("/v0/management/" + ch), (d) => {
      const provider = apiKeyProviderIfConfigured(ch, d);
      if (provider) configuredProviders.add(provider);
    });
  }

  // auth-files: fetch the file list (carrying each file's tier/plan identity)
  // and the per-file models concurrently, then join on file name. For tiered
  // providers (codex, antigravity) we union same-tier files' models into one
  // subgroup so the picker shows e.g. "codex · free" with the de-duplicated set
  // of models that any free-tier auth file supports — authorizing a model there
  // pins the request to that tier via the plugin Scheduler. Files whose tier we
  // can't read join the "supported" untiered bucket (never a real tier).
  await safe(c.get("/v0/management/auth-files"), async (d) => {
    const metas = fromAuthFiles(d);
    // Every auth file implies its provider is configured (an OAuth credential
    // exists on disk/in-memory), regardless of tier — record that so the
    // provider's static-definition entries aren't hidden by filterByConfigured.
    for (const m of metas) {
      if (m.provider) configuredProviders.add(m.provider);
    }
    // Fetch per-file models in parallel (bounded) so a large auth dir doesn't
    // serialize into N round-trips. Failures of individual files are swallowed;
    // a file whose models we couldn't fetch simply contributes nothing.
    const perFile = await Promise.all(
      metas.map((m) =>
        c
          .get("/v0/management/auth-files/models", { params: { name: m.name } })
          .then((r) => ({ meta: m, data: r.data }))
          .catch(() => null),
      ),
    );
    // Track which (tiered) providers the auth-files path actually contributed
    // models for. When a tiered provider has real auth files, its models come
    // from here grouped by tier (codex·team / codex·free / codex·supported) —
    // and the static model-definitions entries for the SAME provider (which
    // carry no group/tier) are dropped in filterByConfigured so the user
    // doesn't see a duplicate bare "codex" group of models that may have no
    // backing auth file. A provider with NO auth files keeps its static
    // definitions (e.g. api-key-only providers), since that's the only source.
    for (const res of perFile) {
      if (!res) continue;
      const fileEntries = fromAuthFileModels(res.meta.provider, res.data);
      for (const e of fileEntries) {
        const provider = (e.provider ?? "").toLowerCase();
        if (TIERED_PROVIDERS.has(provider)) {
          // "supported" bucket for files with no readable tier claim; real
          // tiers keep their plan_type value as the group.
          e.group = res.meta.planType || SUPPORTED_GROUP;
          authFileTieredProviders.add(provider);
        }
        entries.push(e);
      }
    }
  });

  for (const ch of STATIC_CHANNELS) {
    await safe(
      c.get("/v0/management/model-definitions/" + ch),
      (d) => entries.push(...fromModelDefinitions(ch, d)),
    );
  }

  const filtered = filterByConfigured(
    entries,
    configuredProviders,
    selected,
    authFileTieredProviders,
  );
  return normalizeCatalog(filtered);
}

// A picker group: a provider, optionally split by tier (codex free / team /
// supported…). When group is undefined the picker renders the provider as a
// flat group (legacy behavior). When group is set, the provider's models are
// shown under a tier-labeled subgroup so the user can authorize a model under
// a specific tier — the plugin Scheduler honors the chosen tier at runtime.
export interface CatalogGroup {
  provider: string;
  group?: string;
  models: string[];
}

// Build picker groups from the normalized catalog. Adjacent (provider, group)
// buckets collapse into one group with their models merged (normalizeCatalog
// already de-duplicated within a bucket, so the merge is just concatenation).
export function groupByCatalog(catalog: CatalogModel[]): CatalogGroup[] {
  const map = new Map<string, CatalogGroup>();
  for (const c of catalog) {
    const group = c.group ?? "";
    const key = c.provider + "\0" + group;
    let bucket = map.get(key);
    if (!bucket) {
      bucket = { provider: c.provider, models: [] };
      if (group) bucket.group = group;
      map.set(key, bucket);
    }
    bucket.models.push(c.model);
  }
  return Array.from(map.values()).sort((a, b) => {
    if (a.provider !== b.provider) return a.provider.localeCompare(b.provider);
    return (a.group ?? "").localeCompare(b.group ?? "");
  });
}
