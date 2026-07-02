import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { listKeys, deleteKey, rotateKey, resetRPM } from "../api/keys";
import type { KeyPublic, UsageSummary } from "../types";
import PlainKeyModal from "../components/PlainKeyModal";
import { useT, translate } from "../i18n";

function fmtUsd(n: number): string {
  return "$" + n.toFixed(2);
}

// Renders a key's daily/weekly dollar usage against its limits. Empty limits
// (0) show as "不限"; usage at/over a limit is flagged in the danger color so an
// admin can spot a throttled key at a glance.
//
// When the backend reports cache stats (cache-read tokens + non-cache input
// tokens), a third line shows cache spend and hit-rate per window. Hit-rate =
// cacheRead / (cacheRead + input); cache-creation tokens are excluded by the
// backend so this stays a clean "of the input we read, how much was cached".
function UsageCell({ usage }: { usage: UsageSummary }) {
  const t = useT();
  const dailyOver = usage.daily_limit_usd > 0 && usage.daily_usd >= usage.daily_limit_usd;
  const weeklyOver = usage.weekly_limit_usd > 0 && usage.weekly_usd >= usage.weekly_limit_usd;

  const dailyCache = cacheLine(usage.daily_cache_read_tokens, usage.daily_input_tokens, usage.daily_cache_cost_usd);
  const weeklyCache = cacheLine(usage.weekly_cache_read_tokens, usage.weekly_input_tokens, usage.weekly_cache_cost_usd);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 2 }}>
      <span className={dailyOver ? "" : "muted"} style={dailyOver ? { color: "var(--danger)", fontWeight: 600 } : undefined}>
        {t("usage.today")} {fmtUsd(usage.daily_usd)}
        {usage.daily_limit_usd > 0 ? ` / ${fmtUsd(usage.daily_limit_usd)}` : ` / ${t("usage.unlimited")}`}
      </span>
      {dailyCache && <span className="muted" style={{ fontSize: 11 }}>{t("usage.cache")} {dailyCache}</span>}
      <span className={weeklyOver ? "" : "muted"} style={weeklyOver ? { color: "var(--danger)", fontWeight: 600 } : undefined}>
        {t("usage.thisWeek")} {fmtUsd(usage.weekly_usd)}
        {usage.weekly_limit_usd > 0 ? ` / ${fmtUsd(usage.weekly_limit_usd)}` : ` / ${t("usage.unlimited")}`}
      </span>
      {weeklyCache && <span className="muted" style={{ fontSize: 11 }}>{t("usage.cache")} {weeklyCache}</span>}
      {(usage.daily_call_count || usage.weekly_call_count) ? (
        <span className="muted" style={{ fontSize: 11 }}>
          {t("usage.calls")} {usage.daily_call_count ?? 0} / {usage.weekly_call_count ?? 0}
        </span>
      ) : null}
    </div>
  );
}

// Build the "cache spend / hit-rate" suffix for one window. Returns "" when no
// cache activity is reported for that window (so the line is hidden entirely).
function cacheLine(cacheRead?: number, inputTokens?: number, cacheCost?: number): string {
  const cr = cacheRead ?? 0;
  const inp = inputTokens ?? 0;
  if (cr <= 0 && inp <= 0) return "";
  const denom = cr + inp;
  const rate = denom > 0 ? (cr / denom) * 100 : 0;
  const cost = fmtUsd(cacheCost ?? 0);
  return `${cost} · ${translate("usage.hitRate", { rate: rate.toFixed(0) })}`;
}

export default function KeyList() {
  const t = useT();
  const [keys, setKeys] = useState<KeyPublic[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [plain, setPlain] = useState<string | null>(null);
  const [plainTitle, setPlainTitle] = useState<string>("");

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      setKeys(await listKeys());
    } catch (e) {
      const err = e as { response?: { data?: { error?: { message?: string } } }; message?: string };
      setError(err.response?.data?.error?.message ?? err.message ?? t("keys.loadFailed"));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const onRotate = async (id: string) => {
    if (!confirm(t("keys.rotateConfirm", { id }))) return;
    try {
      const r = await rotateKey(id);
      setPlain(r.plain_key);
      setPlainTitle(t("keys.rotated"));
      void load();
    } catch (e) {
      alert((e as Error).message ?? t("keys.rotateFailed"));
    }
  };

  const onReset = async (id: string) => {
    try {
      await resetRPM(id);
      void load();
    } catch (e) {
      alert((e as Error).message ?? t("keys.resetFailed"));
    }
  };

  const onDelete = async (id: string) => {
    if (!confirm(t("keys.deleteConfirm", { id }))) return;
    try {
      await deleteKey(id);
      void load();
    } catch (e) {
      alert((e as Error).message ?? t("keys.deleteFailed"));
    }
  };

  return (
    <div>
      <div className="actions" style={{ marginBottom: 14 }}>
        <Link to="/keys/new"><button className="btn primary">{t("keys.newKey")}</button></Link>
        <button className="btn" onClick={load}>{t("keys.refresh")}</button>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? (
        <div className="muted">{t("keys.loading")}</div>
      ) : keys.length === 0 ? (
        <div className="card muted">{t("keys.empty")}</div>
      ) : (
        <div className="card table-wrap">
          <table>
            <thead>
              <tr>
                <th>{t("keys.colIdName")}</th>
                <th>{t("keys.colStatus")}</th>
                <th>{t("keys.colPreview")}</th>
                <th>{t("keys.colRpm")}</th>
                <th>{t("keys.colUsage")}</th>
                <th>{t("keys.colModels")}</th>
                <th>{t("keys.colActions")}</th>
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id}>
                  <td>
                    <div className="mono">{k.id}</div>
                    <div className="muted">{k.name}</div>
                  </td>
                  <td>
                    <span className={"tag " + (k.enabled ? "on" : "off")}>
                      {k.enabled ? t("keys.enabled") : t("keys.disabled")}
                    </span>
                  </td>
                  <td className="mono">{k.key_preview}</td>
                  <td>{k.rpm}</td>
                  <td>
                    <UsageCell usage={k.usage} />
                  </td>
                  <td>{(k.models ?? []).length}</td>
                  <td>
                    <div className="actions">
                      <Link to={`/keys/${encodeURIComponent(k.id)}/usage`}>
                        <button className="btn sm" title={t("keys.detail")}>{t("keys.detail")}</button>
                      </Link>
                      <Link to={`/keys/${encodeURIComponent(k.id)}/edit`}>
                        <button className="btn sm">{t("keys.edit")}</button>
                      </Link>
                      <button className="btn sm" onClick={() => onReset(k.id)}>{t("keys.resetRpm")}</button>
                      <button className="btn sm" onClick={() => onRotate(k.id)}>{t("keys.rotate")}</button>
                      <button className="btn sm danger" onClick={() => onDelete(k.id)}>{t("keys.delete")}</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={plainTitle}
          onClose={() => setPlain(null)}
        />
      )}
    </div>
  );
}
