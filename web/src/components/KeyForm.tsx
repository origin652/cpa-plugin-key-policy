import { Fragment, useCallback, useEffect, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import type { KeyPublic, ModelRule, AliasMapping, AccountBinding } from "../types";
import ModelPicker from "./ModelPicker";
import { getPriceTable, lookupPrice, type PriceTable } from "../store/modelPrices";
import { fetchAliases } from "../api/mappings";
import { formatTierLabel } from "../api/models";
import { useT } from "../i18n";

export interface KeyFormValues {
  id: string;
  name: string;
  enabled: boolean;
  rpm: number;
  models: ModelRule[];
  daily_limit_usd: number;
  weekly_limit_usd: number;
  // Per-key override for GET /v1/models. CPA cannot filter the model list per
  // downstream key, so the only plugin-enforceable choice is binary: 401 (hide
  // the list) or allow (client sees the full global list). Default false.
  allow_models_endpoint?: boolean;
  account_binding?: AccountBinding;
  clear_account_binding?: boolean;
}

interface Props {
  initial?: KeyPublic;
  idReadOnly?: boolean;
  submitLabel: string;
  onSubmit: (v: KeyFormValues) => Promise<void>;
  onCancel: () => void;
  // top-level error to render
  error?: string;
  // route path for the standalone model-picker page (e.g. "/keys/new/models").
  // When set, the desktop form renders a chip box + "add model" button that
  // navigates here with the current models as router state. The picker page
  // navigates back with state.pickedModels, which the parent merges into
  // `initial` before re-rendering this form.
  pickPath?: string;
  // extra-danger button config for the footer (edit mode). When provided,
  // renders a danger-outline button on the far right of the footer.
  dangerLabel?: string;
  onDanger?: () => void;
}

// Pricing for a single alias, kept in form state alongside the model selection.
interface PriceRow {
  input_price_per_million: number;
  output_price_per_million: number;
  cache_read_price_per_million: number;
  // per-call billing toggle + fixed charge. billing_mode "tokens" (default)
  // uses the three token prices; "per_call" uses per_call_usd per successful
  // request and ignores the token prices (kept dormant for round-tripping).
  billing_mode: "tokens" | "per_call";
  per_call_usd: number;
}

// Price-map key. A model selected under different tiers (codex free vs team)
// produces two ModelRules with the SAME alias but different groups — pricing
// must be tracked per (group, alias) so each row keeps its own numbers. The
// group prefix (lowercased) disambiguates; aliases without a group use the
// alias alone, preserving the legacy key shape for non-tiered providers.
function priceKey(m: { alias: string; group?: string }): string {
  const g = (m.group ?? "").toLowerCase();
  return (g ? g + "|" : "") + m.alias.toLowerCase();
}

function parseNum(value: string): number {
  const n = parseFloat(value);
  return Number.isFinite(n) ? n : 0;
}

export default function KeyForm({
  initial,
  idReadOnly,
  submitLabel,
  onSubmit,
  onCancel,
  error,
  pickPath,
  dangerLabel,
  onDanger,
}: Props) {
  const nav = useNavigate();
  const [id, setId] = useState(initial?.id ?? "");
  const isNative = initial?.native ?? false;
  const [name, setName] = useState(initial?.name ?? "");
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [rpm, setRpm] = useState(initial?.rpm ?? 0);
  const [dailyLimit, setDailyLimit] = useState(initial?.daily_limit_usd ?? 0);
  const [weeklyLimit, setWeeklyLimit] = useState(initial?.weekly_limit_usd ?? 0);
  const [allowModels, setAllowModels] = useState<boolean>(initial?.allow_models_endpoint ?? false);
  const [bindingEnabled, setBindingEnabled] = useState<boolean>(initial?.account_binding !== undefined);
  const [bindingAllow, setBindingAllow] = useState((initial?.account_binding?.allow ?? []).join("\n"));
  const [bindingStrategy, setBindingStrategy] = useState<AccountBinding["strategy"]>(initial?.account_binding?.strategy ?? "weighted-round-robin");
  const t = useT();

  // Pricing table keyed by alias (lowercased) so it survives picker re-emits.
  const [prices, setPrices] = useState<Record<string, PriceRow>>(() => {
    const out: Record<string, PriceRow> = {};
    for (const m of initial?.models ?? []) {
      out[priceKey(m)] = {
        input_price_per_million: m.input_price_per_million ?? 0,
        output_price_per_million: m.output_price_per_million ?? 0,
        cache_read_price_per_million: m.cache_read_price_per_million ?? 0,
        billing_mode: m.billing_mode === "per_call" ? "per_call" : "tokens",
        per_call_usd: m.per_call_usd ?? 0,
      };
    }
    return out;
  });
  const [models, setModels] = useState<ModelRule[]>(initial?.models ?? []);
  const [busy, setBusy] = useState(false);
  const [localErr, setLocalErr] = useState("");
  const [expandedPrice, setExpandedPrice] = useState<Record<string, boolean>>({});

  // Global alias table (fetched once on mount). Used by the "已有别名" section
  // so the user can quickly include an alias's targets instead of picking
  // each provider+model+group individually from the catalog. Clicking a
  // global alias adds ALL its targets as ModelRules (so round-robin can rotate
  // across them); the backend ups-server dedups by alias+provider+target_model
  // so it reuses the existing global alias instead of creating a duplicate.
  const [globalAliases, setGlobalAliases] = useState<AliasMapping[]>([]);
  useEffect(() => {
    let alive = true;
    void fetchAliases().then((list) => { if (alive) setGlobalAliases(list); }).catch(() => {});
    return () => { alive = false; };
  }, []);

  // aliasSelected reports whether every target of `a` is already in `models`
  // (i.e. the alias is fully included).
  const aliasSelected = useCallback((a: AliasMapping) => {
    return a.targets.every((tgt) =>
      models.some((m) =>
        m.alias.toLowerCase() === a.alias.toLowerCase() &&
        m.provider.toLowerCase() === tgt.provider.toLowerCase() &&
        m.target_model.toLowerCase() === tgt.target_model.toLowerCase() &&
        (m.group ?? "").toLowerCase() === (tgt.group ?? "").toLowerCase(),
      ),
    );
  }, [models]);

  // toggleAlias either adds all of an alias's targets (as ModelRules, with the
  // alias's pricing stamped in) or removes all of them.
  const toggleAlias = useCallback((a: AliasMapping) => {
    if (aliasSelected(a)) {
      // Remove all targets of this alias.
      setModels((prev) => prev.filter((m) =>
        !(m.alias.toLowerCase() === a.alias.toLowerCase()),
      ));
      setPrices((prev) => {
        const next = { ...prev };
        for (const tgt of a.targets) {
          const k = priceKey({ alias: a.alias, group: tgt.group });
          delete next[k];
        }
        return next;
      });
    } else {
      // Add all targets.
      const newRules: ModelRule[] = a.targets.map((tgt) => ({
        alias: a.alias,
        provider: tgt.provider,
        target_model: tgt.target_model,
        group: tgt.group ?? "",
        billing_mode: a.billing_mode === "per_call" ? "per_call" : "tokens",
        input_price_per_million: a.input_price_per_million ?? 0,
        output_price_per_million: a.output_price_per_million ?? 0,
        cache_read_price_per_million: a.cache_read_price_per_million ?? 0,
        per_call_usd: a.per_call_usd ?? 0,
      }));
      setModels((prev) => {
        // Drop any partial entries for this alias first, then append all targets.
        const filtered = prev.filter((m) => m.alias.toLowerCase() !== a.alias.toLowerCase());
        return [...filtered, ...newRules];
      });
      setPrices((prev) => {
        const next = { ...prev };
        for (const tgt of a.targets) {
          next[priceKey({ alias: a.alias, group: tgt.group })] = {
            input_price_per_million: a.input_price_per_million ?? 0,
            output_price_per_million: a.output_price_per_million ?? 0,
            cache_read_price_per_million: a.cache_read_price_per_million ?? 0,
            billing_mode: a.billing_mode === "per_call" ? "per_call" : "tokens",
            per_call_usd: a.per_call_usd ?? 0,
          };
        }
        return next;
      });
    }
  }, [aliasSelected]);

  // LiteLLM price hints (community price table). Loaded once on mount, silent
  // failure: if null/inflight, the per-row "recommend" affordance simply isn't
  // rendered. The form is fully usable without it. Never auto-fills prices —
  // the user must click "recommend" per row (replace semantics, overwrites
  // whatever was in that row).
  const [priceTable, setPriceTable] = useState<PriceTable | null>(null);
  useEffect(() => {
    let alive = true;
    void getPriceTable().then((t) => {
      if (alive) setPriceTable(t);
    });
    return () => {
      alive = false;
    };
  }, []);

  // ModelPicker emits fresh ModelRule[] on every selection change (and once
  // when the catalog finishes loading). We must NOT let those re-emits wipe
  // pricing the user already typed: when merging, preserve existing rows and
  // only (a) add empty rows for newly-selected aliases, (b) drop rows for
  // aliases that are no longer selected. Keys already present are copied
  // through untouched. Wrapped in useCallback so ModelPicker's emit effect
  // does not re-fire on every KeyForm re-render (which would otherwise loop
  // and risk dropping mid-typing values).
  const handleModelsChange = useCallback((next: ModelRule[]) => {
    setModels(next);
    setPrices((prev) => {
      const updated: Record<string, PriceRow> = {};
      for (const m of next) {
        const key = priceKey(m);
        updated[key] = prev[key] ?? { input_price_per_million: 0, output_price_per_million: 0, cache_read_price_per_million: 0, billing_mode: "tokens", per_call_usd: 0 };
      }
      // Rows for (group,alias) pairs no longer selected simply aren't copied.
      return updated;
    });
  }, []);

  const setPrice = (m: ModelRule, field: keyof PriceRow, value: string) => {
    const key = priceKey(m);
    setPrices((prev) => ({
      ...prev,
      [key]: {
        ...(prev[key] ?? { input_price_per_million: 0, output_price_per_million: 0, cache_read_price_per_million: 0, billing_mode: "tokens", per_call_usd: 0 }),
        [field]: field === "billing_mode" ? (value === "per_call" ? "per_call" : "tokens") : parseNum(value),
      },
    }));
  };

  // One-click fill this row from LiteLLM community prices. Replace semantics:
  // overwrites all three fields (even non-zero user-entered ones). Lookup is by
  // target_model (the real upstream id); the price writes back to this row's
  // (group, alias) key, so a same-alias row under a different tier is untouched.
  const recommend = (m: ModelRule) => {
    const row = lookupPrice(priceTable, m.target_model);
    if (!row) return;
    const key = priceKey(m);
    setPrices((prev) => ({
      ...prev,
      [key]: {
        input_price_per_million: row.input_price_per_million,
        output_price_per_million: row.output_price_per_million,
        cache_read_price_per_million: row.cache_read_price_per_million,
        // Recommend fills token prices; it does not change the billing mode. If
        // the row was on per_call, keep it (the recommended token prices stay
        // dormant until the user switches back to tokens).
        billing_mode: prev[key]?.billing_mode ?? "tokens",
        per_call_usd: prev[key]?.per_call_usd ?? 0,
      },
    }));
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLocalErr("");
    if (!id.trim()) {
      setLocalErr(t("keyForm.idRequired"));
      return;
    }
    // Stamp the per-alias pricing back onto the model rules before submit.
    const pricedModels: ModelRule[] = models.map((m) => {
      const row = prices[priceKey(m)];
      return {
        ...m,
        input_price_per_million: row?.input_price_per_million ?? 0,
        output_price_per_million: row?.output_price_per_million ?? 0,
        cache_read_price_per_million: row?.cache_read_price_per_million ?? 0,
        billing_mode: row?.billing_mode === "per_call" ? "per_call" : "tokens",
        per_call_usd: row?.per_call_usd ?? 0,
      };
    });
    setBusy(true);
    try {
	  const accountBinding = bindingEnabled ? {
	    allow: bindingAllow.split(/\r?\n/).map((value) => value.trim()).filter(Boolean),
	    strategy: bindingStrategy,
	  } satisfies AccountBinding : undefined;
      await onSubmit({
        id: id.trim(),
        name: name.trim(),
        enabled,
        rpm,
        models: pricedModels,
        daily_limit_usd: dailyLimit,
        weekly_limit_usd: weeklyLimit,
        allow_models_endpoint: allowModels,
		account_binding: accountBinding,
		clear_account_binding: !isNative && !bindingEnabled && initial?.account_binding !== undefined,
      });
    } catch (err) {
      const e = err as { response?: { data?: { error?: { message?: string } } }; message?: string };
      setLocalErr(e.response?.data?.error?.message ?? e.message ?? t("keyForm.submitFailed"));
    } finally {
      setBusy(false);
    }
  };

  const toggleExpanded = (key: string) => {
    setExpandedPrice((prev) => ({ ...prev, [key]: !prev[key] }));
  };

  const renderPriceEditor = (m: ModelRule, layout: "table" | "mobile") => {
    const key = priceKey(m);
    // All ModelRules sharing this (group|alias) key — for a multi-target
    // global alias they all share one unified price row. The desktop table
    // renders one row per unique key (see the body filter below) and shows
    // the aggregated provider/group set so the user sees it's one unified
    // price across multiple targets, not a per-target price.
    const sameKey = models.filter((x) => priceKey(x) === key);
    const isMulti = sameKey.length > 1;
      const providersUnion = Array.from(new Set(sameKey.map((x) => x.provider))).join(", ");
      const groupsUnionRaw = Array.from(new Set(sameKey.map((x) => (x.group ?? "").trim()).filter(Boolean)));
      const groupsUnion = groupsUnionRaw.length
        ? groupsUnionRaw.map((g) => formatTierLabel(t, g)).join(", ")
        : "—";
    const row = prices[key] ?? {
      input_price_per_million: 0,
      output_price_per_million: 0,
      cache_read_price_per_million: 0,
      billing_mode: "tokens" as const,
      per_call_usd: 0,
    };
    const perCall = row.billing_mode === "per_call";
    // Community price hint only makes sense for a single target (one
    // target_model); for multi-target aliases the price is unified across
    // different models so auto-fill is meaningless.
    const hint = priceTable && !isMulti ? lookupPrice(priceTable, m.target_model) : null;

    if (layout === "mobile") {
      return (
        <div className="kf-mprice-body">
          <div className="seg kf-billing-seg" role="group" aria-label={t("keyForm.colBillingMode")}>
            <button
              type="button"
              className={"seg-btn" + (perCall ? "" : " active")}
              onClick={() => setPrice(m, "billing_mode", "tokens")}
            >
              {t("keyForm.billingTokens")}
            </button>
            <button
              type="button"
              className={"seg-btn" + (perCall ? " active" : "")}
              onClick={() => setPrice(m, "billing_mode", "per_call")}
            >
              {t("keyForm.billingPerCall")}
            </button>
          </div>
          {perCall ? (
            <div className="form-row">
              <label>{t("keyForm.colPerCall")}</label>
              <input
                className="input"
                type="number"
                min={0}
                step="0.0001"
                value={row.per_call_usd}
                onChange={(e) => setPrice(m, "per_call_usd", e.target.value)}
              />
            </div>
          ) : (
            <>
              <div className="form-row">
                <label>{t("keyForm.colInput")}</label>
                <input
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={row.input_price_per_million}
                  onChange={(e) => setPrice(m, "input_price_per_million", e.target.value)}
                />
              </div>
              <div className="form-row">
                <label>{t("keyForm.colOutput")}</label>
                <input
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={row.output_price_per_million}
                  onChange={(e) => setPrice(m, "output_price_per_million", e.target.value)}
                />
              </div>
              <div className="form-row">
                <label title={t("keyForm.colCacheReadHint")}>{t("keyForm.colCacheRead")}</label>
                <input
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={row.cache_read_price_per_million}
                  onChange={(e) => setPrice(m, "cache_read_price_per_million", e.target.value)}
                />
              </div>
              {hint && (
                <button
                  type="button"
                  className="btn sm"
                  onClick={() => recommend(m)}
                  title={t("keyForm.recommendTitle")}
                >
                  {t("keyForm.recommend")}
                </button>
              )}
            </>
          )}
          {perCall && row.per_call_usd === 0 && (
            <p className="muted kf-warn">⚠ {t("keyForm.perCallZeroWarn")}</p>
          )}
          {perCall && <p className="muted kf-warn">⚠ {t("keyForm.perCallImageWarn")}</p>}
        </div>
      );
    }

    return (
      <Fragment key={key}>
        <tr>
          <td className="mono">{m.alias}{isMulti ? ` (${sameKey.length})` : ""}</td>
          <td className="muted">{isMulti ? providersUnion : m.provider}</td>
          <td className="muted">{isMulti ? groupsUnion : (m.group ? formatTierLabel(t, m.group) : "—")}</td>
          <td>
            <label className="switch" title={t("keyForm.billingModeTitle")}>
              <input
                type="checkbox"
                checked={perCall}
                onChange={(e) => setPrice(m, "billing_mode", e.target.checked ? "per_call" : "tokens")}
              />
              <span className="track"><span className="thumb" /></span>
              <span>{perCall ? t("keyForm.billingPerCall") : t("keyForm.billingTokens")}</span>
            </label>
          </td>
          {perCall ? (
            <td colSpan={3}>
              <div className="form-row" style={{ marginBottom: 0 }}>
                <label>{t("keyForm.colPerCall")}</label>
                <input
                  className="input"
                  type="number"
                  min={0}
                  step="0.0001"
                  value={row.per_call_usd}
                  onChange={(e) => setPrice(m, "per_call_usd", e.target.value)}
                />
              </div>
            </td>
          ) : (
            <>
              <td>
                <input
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={row.input_price_per_million}
                  onChange={(e) => setPrice(m, "input_price_per_million", e.target.value)}
                />
              </td>
              <td>
                <input
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={row.output_price_per_million}
                  onChange={(e) => setPrice(m, "output_price_per_million", e.target.value)}
                />
              </td>
              <td>
                <input
                  className="input"
                  type="number"
                  min={0}
                  step="0.01"
                  value={row.cache_read_price_per_million}
                  onChange={(e) => setPrice(m, "cache_read_price_per_million", e.target.value)}
                />
              </td>
            </>
          )}
          <td>
            {!perCall && hint && (
              <button
                type="button"
                className="btn sm"
                onClick={() => recommend(m)}
                title={t("keyForm.recommendTitle")}
              >
                {t("keyForm.recommend")}
              </button>
            )}
          </td>
        </tr>
        {perCall && row.per_call_usd === 0 && (
          <tr className="muted">
            <td colSpan={8} style={{ fontSize: "0.85em" }}>
              ⚠ {t("keyForm.perCallZeroWarn")}
            </td>
          </tr>
        )}
        {perCall && (
          <tr className="muted">
            <td colSpan={8} style={{ fontSize: "0.85em" }}>
              ⚠ {t("keyForm.perCallImageWarn")}
            </td>
          </tr>
        )}
      </Fragment>
    );
  };

  const section = (title: string, children: ReactNode) => (
    <section className="kf-section mobile-only">
      <div className="section-label">{title}</div>
      <div className="kf-section-card">{children}</div>
    </section>
  );

  return (
    <form className="card key-form" onSubmit={submit}>
      <div className="mobile-only kf-sections">
        {section(t("keyForm.mobile.sectionBasic"), (
          <>
            <div className="form-row">
              <label>{t("keyForm.idLabel")}</label>
              <input
                className={"input" + (idReadOnly ? " mono" : "")}
                value={id}
                onChange={(e) => setId(e.target.value)}
                readOnly={idReadOnly}
                placeholder={t("keyForm.idPlaceholder")}
                autoFocus={!idReadOnly}
              />
            </div>
            <div className="form-row">
              <label>{t("keyForm.nameLabel")}</label>
              <input
                className="input"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={t("keyForm.namePlaceholder")}
              />
            </div>
            <div className="form-row kf-switch-row">
              <label className="switch">
                <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
                <span className="track"><span className="thumb" /></span>
                <span>{t("keyForm.enableKey")}</span>
              </label>
            </div>
            {!isNative && <div className="form-row">
              <label>{t("keyForm.rpmLabel")}</label>
              <input
                className="input"
                type="number"
                min={0}
                value={rpm}
                onChange={(e) => setRpm(parseInt(e.target.value || "0", 10) || 0)}
              />
            </div>}
          </>
        ))}
        {!isNative && section(t("keyForm.mobile.sectionLimits"), (
          <>
            <div className="form-row">
              <label>{t("keyForm.dailyLimitLabel")}</label>
              <input
                className="input"
                type="number"
                min={0}
                step="0.01"
                value={dailyLimit}
                onChange={(e) => setDailyLimit(parseNum(e.target.value))}
              />
            </div>
            <div className="form-row">
              <label>{t("keyForm.weeklyLimitLabel")}</label>
              <input
                className="input"
                type="number"
                min={0}
                step="0.01"
                value={weeklyLimit}
                onChange={(e) => setWeeklyLimit(parseNum(e.target.value))}
              />
            </div>
          </>
        ))}
        {section(t("keyForm.mobile.sectionAccess"), (
          <>
            {!isNative && <><label className="switch kf-access-switch" title={t("keyForm.allowModelsTitle")}>
              <input type="checkbox" checked={allowModels} onChange={(e) => setAllowModels(e.target.checked)} />
              <span className="track"><span className="thumb" /></span>
              <span>{t("keyForm.allowModelsLabel")}</span>
            </label>
            <p className="muted kf-hint">{t("keyForm.allowModelsHint")}</p></>}
            <label className="switch kf-access-switch" title={t("keyForm.accountBindingTitle")}>
              <input type="checkbox" checked={bindingEnabled} disabled={isNative} onChange={(e) => setBindingEnabled(e.target.checked)} />
              <span className="track"><span className="thumb" /></span>
              <span>{t("keyForm.accountBindingLabel")}</span>
            </label>
            {isNative && <p className="muted kf-hint">{t("keyForm.nativeBindingHint")}</p>}
            {bindingEnabled && (
              <>
                <div className="form-row">
                  <label>{t("keyForm.accountBindingAllowLabel")}</label>
                  <textarea className="input mono" rows={4} value={bindingAllow} onChange={(e) => setBindingAllow(e.target.value)} placeholder={t("keyForm.accountBindingAllowPlaceholder")} />
                </div>
                <div className="form-row">
                  <label>{t("keyForm.accountBindingStrategyLabel")}</label>
                  <select className="input" value={bindingStrategy} onChange={(e) => setBindingStrategy(e.target.value as AccountBinding["strategy"])}>
                    <option value="weighted-round-robin">weighted-round-robin</option>
                    <option value="round-robin">round-robin</option>
                    <option value="fill-first">fill-first</option>
                  </select>
                </div>
                <p className="muted kf-hint">{t("keyForm.accountBindingHint")}</p>
              </>
            )}
          </>
        ))}
        {!isNative && <section className="kf-section mobile-only">
          <div className="section-label">{t("keyForm.mobile.sectionModels")}</div>
          {globalAliases.length > 0 && (
            <div className="form-row kf-alias-pick" style={{ marginBottom: 12 }}>
              <div className="kf-alias-chips">
                {globalAliases.map((a) => {
                  const on = aliasSelected(a);
                  return (
                  <button key={a.alias} type="button" className={"kf-alias-chip" + (on ? " selected" : "")} onClick={() => toggleAlias(a)}>
                    {a.alias}{a.targets.length > 1 ? ` (${a.targets.length})` : ""}
                  </button>
                  );
                })}
              </div>
            </div>
          )}
          <div className="form-row" style={{ marginBottom: 12 }}>
            {pickPath ? (
              <div className="model-chips-box">
                {models.length === 0 && <span className="mc-empty">{t("keyForm.modelsEmpty")}</span>}
                {models.map((m) => (
                  <span key={priceKey(m)} className="mc-chip">
                    {m.alias}{m.group ? " · " + formatTierLabel(t, m.group) : ""}
                    <button type="button" className="mc-x" onClick={() => {
                      setModels((prev) => prev.filter((x) => priceKey(x) !== priceKey(m)));
                    }} aria-label={t("keyForm.removeModel")}>×</button>
                  </span>
                ))}
                <button type="button" className="mc-add" onClick={() => nav(pickPath, { state: { models } })}>
                  + {t("keyForm.addModel")}
                </button>
              </div>
            ) : (
              <ModelPicker initial={initial?.models} onChange={handleModelsChange} />
            )}
          </div>
          {models.length > 0 && (
            <div className="kf-model-list">
              {models.map((m) => {
                const key = priceKey(m);
                const row = prices[key];
                const perCall = row?.billing_mode === "per_call";
                const open = !!expandedPrice[key];
                return (
                  <div key={key} className="kf-model-card">
                    <button
                      type="button"
                      className="kf-model-head"
                      onClick={() => toggleExpanded(key)}
                      aria-expanded={open}
                    >
                      <div>
                        <div className="kf-model-alias">{m.alias}</div>
                        <div className="muted kf-model-meta">
                          {m.provider}{m.group ? ` · ${formatTierLabel(t, m.group)}` : ""}
                        </div>
                        <div className="mono kf-model-target">{m.target_model}</div>
                      </div>
                      <span className={"mm-badge" + (perCall ? " per_call" : "")}>
                        {perCall ? t("keyForm.billingPerCall") : t("keyForm.billingTokens")}
                      </span>
                      <span className="kf-chevron">{open ? "▾" : "▸"}</span>
                    </button>
                    {open && renderPriceEditor(m, "mobile")}
                  </div>
                );
              })}
            </div>
          )}
          <p className="muted kf-hint" style={{ marginTop: 8 }}>{t("keyForm.priceLabel")}</p>
        </section>}
      </div>

      <div className="mobile-hidden">
      <div className="row2">
        <div className="form-row">
          <label>{t("keyForm.idLabel")}</label>
          <input
            className="input"
            value={id}
            onChange={(e) => setId(e.target.value)}
            readOnly={idReadOnly}
            placeholder={t("keyForm.idPlaceholder")}
            autoFocus={!idReadOnly}
          />
        </div>
        <div className="form-row">
          <label>{t("keyForm.nameLabel")}</label>
          <input
            className="input"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t("keyForm.namePlaceholder")}
          />
        </div>
      </div>
      <div className="row2">
        {!isNative && <div className="form-row">
          <label>{t("keyForm.rpmLabel")}</label>
          <input
            className="input"
            type="number"
            min={0}
            value={rpm}
            onChange={(e) => setRpm(parseInt(e.target.value || "0", 10) || 0)}
          />
        </div>}
        <div className="form-row">
          <label>{t("keyForm.statusLabel")}</label>
          <label className="switch">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />
            <span className="track"><span className="thumb" /></span>
            <span>{t("keyForm.enableKey")}</span>
          </label>
        </div>
      </div>

      {!isNative && <div className="row2">
        <div className="form-row">
          <label>{t("keyForm.dailyLimitLabel")}</label>
          <input
            className="input"
            type="number"
            min={0}
            step="0.01"
            value={dailyLimit}
            onChange={(e) => setDailyLimit(parseNum(e.target.value))}
          />
        </div>
        <div className="form-row">
          <label>{t("keyForm.weeklyLimitLabel")}</label>
          <input
            className="input"
            type="number"
            min={0}
            step="0.01"
            value={weeklyLimit}
            onChange={(e) => setWeeklyLimit(parseNum(e.target.value))}
          />
        </div>
      </div>}

      {!isNative && <div className="form-row">
        <label className="switch" title={t("keyForm.allowModelsTitle")}>
          <input
            type="checkbox"
            checked={allowModels}
            onChange={(e) => setAllowModels(e.target.checked)}
          />
          <span className="track"><span className="thumb" /></span>
          <span>{t("keyForm.allowModelsLabel")}</span>
        </label>
        <span className="muted" style={{ fontSize: "0.85em", marginLeft: 8 }}>
          {t("keyForm.allowModelsHint")}
        </span>
      </div>}

      <div className="form-row">
        <label className="switch" title={t("keyForm.accountBindingTitle")}>
          <input type="checkbox" checked={bindingEnabled} disabled={isNative} onChange={(e) => setBindingEnabled(e.target.checked)} />
          <span className="track"><span className="thumb" /></span>
          <span>{t("keyForm.accountBindingLabel")}</span>
        </label>
        {isNative && <span className="muted" style={{ fontSize: "0.85em", marginLeft: 8 }}>{t("keyForm.nativeBindingHint")}</span>}
        {bindingEnabled && (
          <div className="row2" style={{ marginTop: 10 }}>
            <div className="form-row">
              <label>{t("keyForm.accountBindingAllowLabel")}</label>
              <textarea className="input mono" rows={4} value={bindingAllow} onChange={(e) => setBindingAllow(e.target.value)} placeholder={t("keyForm.accountBindingAllowPlaceholder")} />
              <span className="muted" style={{ fontSize: "0.85em" }}>{t("keyForm.accountBindingHint")}</span>
            </div>
            <div className="form-row">
              <label>{t("keyForm.accountBindingStrategyLabel")}</label>
              <select className="input" value={bindingStrategy} onChange={(e) => setBindingStrategy(e.target.value as AccountBinding["strategy"])}>
                <option value="weighted-round-robin">weighted-round-robin</option>
                <option value="round-robin">round-robin</option>
                <option value="fill-first">fill-first</option>
              </select>
            </div>
          </div>
        )}
      </div>

      {!isNative && globalAliases.length > 0 && (
        <div className="form-row kf-alias-pick">
          <label>{t("keyForm.existingAliases")}</label>
          <div className="kf-alias-chips">
            {globalAliases.map((a) => {
              const on = aliasSelected(a);
              return (
              <button
                key={a.alias}
                type="button"
                className={"kf-alias-chip" + (on ? " selected" : "")}
                onClick={() => toggleAlias(a)}
                title={a.targets.map((t) => `${t.provider}·${t.target_model}${t.group ? `·${t.group}` : ""}`).join("\n")}
              >
                {a.alias}{a.targets.length > 1 ? ` (${a.targets.length})` : ""}
              </button>
              );
            })}
          </div>
        </div>
      )}

      {!isNative && <div className="form-row">
        <label>{t("keyForm.modelsLabel")}</label>
        {pickPath ? (
          <div className="model-chips-box">
            {models.length === 0 && <span className="mc-empty">{t("keyForm.modelsEmpty")}</span>}
            {models.map((m) => (
              <span key={priceKey(m)} className="mc-chip">
                {m.alias}{m.group ? " · " + m.group : ""}
                <button type="button" className="mc-x" onClick={() => {
                  setModels((prev) => prev.filter((x) => priceKey(x) !== priceKey(m)));
                }} aria-label={t("keyForm.removeModel")}>×</button>
              </span>
            ))}
            <button type="button" className="mc-add" onClick={() => nav(pickPath, { state: { models } })}>
              + {t("keyForm.addModel")}
            </button>
          </div>
        ) : (
          <ModelPicker initial={initial?.models} onChange={handleModelsChange} />
        )}
      </div>}

      {/* Per-alias pricing table. Stamped onto each ModelRule at submit.
          Each row toggles between token pricing (default) and per-call fixed
          pricing. Under per_call the three token-price inputs are hidden
          (values retained but dormant) and a single $/call input is shown. */}
      {!isNative && models.length > 0 && (
        <div className="form-row" style={{ marginTop: 8 }}>
          <label>{t("keyForm.priceLabel")}</label>
          <div className="card table-wrap" style={{ padding: 0 }}>
            <table>
              <thead>
                <tr>
                  <th>{t("keyForm.colAlias")}</th>
                  <th>{t("keyForm.colProvider")}</th>
                  <th>{t("keyForm.colGroup")}</th>
                  <th title={t("keyForm.colBillingModeHint")}>{t("keyForm.colBillingMode")}</th>
                  <th>{t("keyForm.colInput")}</th>
                  <th>{t("keyForm.colOutput")}</th>
                  <th title={t("keyForm.colCacheReadHint")}>{t("keyForm.colCacheRead")}</th>
                  <th title={t("keyForm.colRecommendHint")}>{t("keyForm.colRecommend")}</th>
                </tr>
              </thead>
              <tbody>
                {/* Per-alias unified pricing: dedupe by priceKey so a
                    multi-target global alias renders ONE unified price row
                    (aggregated provider/group show the targets it covers). */}
                {models.filter((m, i, arr) => arr.findIndex((x) => priceKey(x) === priceKey(m)) === i).map((m) => renderPriceEditor(m, "table"))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      </div>

      {(localErr || error) && <div className="error">{localErr || error}</div>}

      <div className="actions fp-foot">
        <button className="btn primary" type="submit" disabled={busy}>
          {busy ? t("keyForm.submitting") : submitLabel}
        </button>
        <button className="btn" type="button" onClick={onCancel}>{t("keyForm.cancel")}</button>
        {dangerLabel && onDanger && (
          <span className="fp-foot-right">
            <button type="button" className="btn danger-outline" onClick={onDanger}>{dangerLabel}</button>
          </span>
        )}
      </div>
    </form>
  );
}
