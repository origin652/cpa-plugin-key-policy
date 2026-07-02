import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { fetchKeyUsage } from "../api/keys";
import type { AliasUsageEntry, KeyUsageResponse, UsageWindow } from "../types";
import { useT } from "../i18n";

// Window switch for the per-alias breakdown table: each alias row has its own
// daily and rolling-weekly window, and the user toggles which one all rows
// show at once. Mirrors the KeyList usage column's today/this-week framing.
type Window = "daily" | "weekly";

function fmtUsd(n: number): string {
  return "$" + (Number.isFinite(n) ? n.toFixed(2) : "0.00");
}

// Compact integer formatting with thousands separators. 0 shows as "0".
function fmtInt(n: number): string {
  if (!n || n <= 0) return "0";
  return Math.round(n).toLocaleString("en-US");
}

// Hit-rate = cacheRead / (cacheRead + input), expressed as a percentage.
// Returns "—" when there's no input activity for the window (avoid 0/0).
function hitRate(w: UsageWindow): string {
  const cr = w.cache_read_tokens ?? 0;
  const inp = w.input_tokens ?? 0;
  const denom = cr + inp;
  if (denom <= 0) return "—";
  return Math.round((cr / denom) * 100) + "%";
}

// Billing-mode tag, reusing the existing .tag styling. Per-call rows use a
// distinct tint so they're scannable at a glance.
function BillingTag({ mode }: { mode?: string }) {
  const t = useT();
  const perCall = mode === "per_call";
  return (
    <span className={"tag " + (perCall ? "off" : "on")} style={perCall ? { color: "var(--accent)", borderColor: "var(--accent-ring)", background: "var(--accent-soft)" } : undefined}>
      {perCall ? t("keyUsage.billingPerCall") : t("keyUsage.billingTokens")}
    </span>
  );
}

export default function KeyUsage() {
  const { id } = useParams<{ id: string }>();
  const t = useT();
  const [data, setData] = useState<KeyUsageResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [win, setWin] = useState<Window>("daily");

  useEffect(() => {
    let alive = true;
    (async () => {
      setLoading(true);
      setError("");
      try {
        const keyId = decodeURIComponent(id ?? "");
        if (!keyId) {
          setError(t("keyUsage.notFound"));
          return;
        }
        setData(await fetchKeyUsage(keyId));
      } catch (e) {
        const err = e as { response?: { data?: { error?: { message?: string } } }; message?: string };
        setError(err.response?.data?.error?.message ?? err.message ?? t("keyUsage.loadFailed"));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  if (loading) return <div className="muted">{t("keyUsage.loading")}</div>;
  if (error || !data) return <div className="error">{error || t("keyUsage.notFound")}</div>;

  const aliases = data.aliases ?? [];
  const hasUsage = aliases.some((a) => (a.daily.call_count ?? 0) > 0 || (a.weekly.call_count ?? 0) > 0 || (a.daily.total_usd ?? 0) > 0 || (a.weekly.total_usd ?? 0) > 0);

  const windowOf = (a: AliasUsageEntry): UsageWindow => (win === "daily" ? a.daily : a.weekly);

  return (
    <div>
      {/* Header: back · key id (mono) · name · daily/weekly toggle */}
      <div className="keyusage-header">
        <div className="keyusage-idline">
          <Link to="/keys">
            <button className="btn sm">{t("keyUsage.back")}</button>
          </Link>
          <span className="mono keyusage-id">{data.key_id}</span>
          <span className="muted">{data.key_name}</span>
        </div>
        <div className="keyusage-toggle" role="tablist" aria-label={t("keyUsage.windowToggle")}>
          <button
            role="tab"
            aria-selected={win === "daily"}
            className={"btn sm " + (win === "daily" ? "primary" : "")}
            onClick={() => setWin("daily")}
          >
            {t("keyUsage.tabDaily")}
          </button>
          <button
            role="tab"
            aria-selected={win === "weekly"}
            className={"btn sm " + (win === "weekly" ? "primary" : "")}
            onClick={() => setWin("weekly")}
          >
            {t("keyUsage.tabWeekly")}
          </button>
        </div>
      </div>

      {/* Card with the per-alias table. Empty-state hint sits above the table
          when no alias has any recorded usage, but configured aliases are
          still listed as zero rows (per the design spec). */}
      <div className="card table-wrap">
        {!hasUsage && <div className="muted keyusage-empty">{t("keyUsage.empty")}</div>}
        <table>
          <thead>
            <tr>
              <th>{t("keyUsage.colAlias")}</th>
              <th>{t("keyUsage.colBillingMode")}</th>
              <th>{t("keyUsage.colProvider")}</th>
              <th className="num">{t("keyUsage.colUsd")}</th>
              <th className="num">{t("keyUsage.colCalls")}</th>
              <th className="num">{t("keyUsage.colInput")}</th>
              <th className="num">{t("keyUsage.colOutput")}</th>
              <th className="num">{t("keyUsage.colCache")}</th>
              <th className="num">{t("keyUsage.colHitRate")}</th>
            </tr>
          </thead>
          <tbody>
            {aliases.length === 0 ? (
              <tr>
                <td colSpan={9} className="muted keyusage-noalias">
                  {t("keyUsage.noAlias")}
                </td>
              </tr>
            ) : (
              aliases.map((a) => {
                const w = windowOf(a);
                return (
                  <tr key={a.alias} className={a.in_config ? "" : "keyusage-residual"}>
                    <td>
                      <div className="mono">{a.alias}</div>
                      {!a.in_config && <span className="keyusage-badge">{t("keyUsage.notInConfig")}</span>}
                    </td>
                    <td>
                      <BillingTag mode={a.billing_mode} />
                    </td>
                    <td className="muted">{a.provider || "—"}</td>
                    <td className="num strong">{fmtUsd(w.total_usd ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.call_count ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.input_tokens ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.output_tokens ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.cache_read_tokens ?? 0)}</td>
                    <td className="num mono">{hitRate(w)}</td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
