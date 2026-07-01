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
      const key = provider + "" + group + "" + model.toLowerCase();
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

function fromChannelKey(channel: string, payload: unknown): RawEntry[] {
  // CPA per-channel endpoints vary; try common shapes.
  const o = payload as Record<string, unknown> | null;
  if (!o) return [];
  // Could be { models: [...] } or an array directly under a key.
  const models = o["models"] ?? o["keys"];
  return [{ provider: channel, models }];
}

// One auth-file row from /v0/management/auth-files. We only need the file name
// (to join against per-file models) and the tier identity claim (codex
// id_token plan_type, antigravity tier). The ListAuthFiles response exposes
// plan_type under id_token.claims.plan_type; the flat "account_type" sibling is
// a fallback. Everything else is ignored.
interface AuthFileMeta {
  name: string;
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
    const planType = readPlanType(o);
    out.push({ name, planType });
  }
  return out;
}

// Extract the tier/plan identity from an auth-files list entry. codex puts it
// under id_token.claims.plan_type; antigravity under a top-level tier. Returns
// "" when no recognizable claim is present (→ the file joins the "supported"
// untiered bucket, never a real tier).
function readPlanType(entry: Record<string, unknown>): string {
  const idToken = entry["id_token"];
  if (idToken && typeof idToken === "object") {
    const claims = (idToken as Record<string, unknown>)["claims"];
    if (claims && typeof claims === "object") {
      const plan = (claims as Record<string,unknown>)["plan_type"];
      if (typeof plan === "string" && plan.trim() !== "") {
        return plan.trim().toLowerCase();
      }
    }
  }
  // antigravity exposes tier identity at the top level on some builds.
  const tier = entry["tier"];
  if (typeof tier === "string" && tier.trim() !== "") {
    return tier.trim().toLowerCase();
  }
  return "";
}

function fromAuthFileModels(name: string, payload: unknown): RawEntry[] {
  const root = payload as Record<string, unknown> | null;
  const models = root?.["models"] ?? root?.["available_models"];
  const provider =
    ((root?.["channel"] as string) ?? (root?.["provider"] as string) ?? "").trim() ||
    name;
  return [{ provider, models }];
}

function fromModelDefinitions(channel: string, payload: unknown): RawEntry[] {
  const root = payload as Record<string, unknown> | null;
  const models = root?.["models"] ?? root?.["definitions"];
  return [{ provider: channel, models }];
}

// Fetch the composed catalog. Failures of individual sources are swallowed so
// that one unavailable endpoint doesn't blank the whole picker. A 401/403 here
// is real (bad key) and surfaces through the shared client as a forced
// re-login — that's intended; we don't mask auth failures.
export async function fetchCatalog(): Promise<CatalogModel[]> {
  const c = apiClient();
  const entries: RawEntry[] = [];

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
    (d) => entries.push(...fromOpenAICompat(d)),
  );

  for (const ch of ["gemini-api-key", "claude-api-key", "codex-api-key", "vertex-api-key"]) {
    await safe(
      c.get("/v0/management/" + ch),
      (d) => entries.push(...fromChannelKey(ch, d)),
    );
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
    for (const res of perFile) {
      if (!res) continue;
      const fileEntries = fromAuthFileModels(res.meta.name, res.data);
      for (const e of fileEntries) {
        const provider = (e.provider ?? "").toLowerCase();
        if (TIERED_PROVIDERS.has(provider)) {
          // "supported" bucket for files with no readable tier claim; real
          // tiers keep their plan_type value as the group.
          e.group = res.meta.planType || SUPPORTED_GROUP;
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

  return normalizeCatalog(entries);
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
