import { useCallback, useEffect, useState } from "react";
import {
  listNativeIdentities,
  listNativePolicies,
  saveNativePolicy,
} from "../api/nativeAccess";
import { fetchCatalog } from "../api/models";
import type { CatalogModel, NativeGrant, NativeIdentity, NativePolicy } from "../types";
import { useT } from "../i18n";

const emptyGrant = (): NativeGrant => ({ provider: "", model: "" });
const numberValue = (value: string): number => {
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
};

export default function NativeAccess() {
  const t = useT();
  const [identities, setIdentities] = useState<NativeIdentity[]>([]);
  const [policies, setPolicies] = useState<NativePolicy[]>([]);
  const [catalog, setCatalog] = useState<CatalogModel[]>([]);
  const [editing, setEditing] = useState<NativePolicy | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const [nextIdentities, nextPolicies, nextCatalog] = await Promise.all([
        listNativeIdentities(),
        listNativePolicies(),
        fetchCatalog(),
      ]);
      setIdentities(nextIdentities);
      setPolicies(nextPolicies);
      setCatalog(nextCatalog);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const edit = (identity: NativeIdentity) => {
    const current = policies.find((item) => item.key_hash === identity.key_hash);
    setEditing(current ? structuredClone(current) : {
      key_hash: identity.key_hash,
      enabled: true,
      grants: [emptyGrant()],
    });
  };

  return (
    <div>
      <div className="fp-head" style={{ margin: "0 0 16px" }}>
        <div>
          <h1>{t("native.title")}</h1>
          <div className="muted">{t("native.identityHint")}</div>
        </div>
        <button className="btn sm" onClick={() => void load()}>{t("keys.refresh")}</button>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? <div className="muted">{t("keys.loading")}</div> : (
        <div className="card-stack">
          {identities.map((identity) => {
            const policy = policies.find((item) => item.key_hash === identity.key_hash);
            return (
              <div className="card" key={identity.key_hash}>
                <div className="fp-head">
                  <div>
                    <strong>{identity.key_preview}</strong>
                    <div className="muted mono">{identity.key_hash.slice(0, 19)}…</div>
                  </div>
                  <span className={"badge " + (policy?.enabled ? "success" : "")}>
                    {policy ? (policy.enabled ? t("keys.enabled") : t("keys.disabled")) : t("native.unmanaged")}
                  </span>
                </div>
                <div className="chip-row">
                  {(policy?.grants ?? []).map((grant, index) => (
                    <span className="chip" key={`${grant.provider}-${grant.model}-${index}`}>
                      {grant.provider} · {grant.model}
                    </span>
                  ))}
                </div>
                <button className="btn sm" onClick={() => edit(identity)}>{t("native.edit")}</button>
              </div>
            );
          })}
        </div>
      )}
      {editing && (
        <NativePolicyEditor
          policy={editing}
          catalog={catalog}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); void load(); }}
        />
      )}
    </div>
  );
}

function NativePolicyEditor({
  policy: initial,
  catalog,
  onClose,
  onSaved,
}: {
  policy: NativePolicy;
  catalog: CatalogModel[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const t = useT();
  const [policy, setPolicy] = useState(initial);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const updateGrant = (index: number, patch: Partial<NativeGrant>) => {
    setPolicy((current) => ({
      ...current,
      grants: current.grants.map((grant, grantIndex) =>
        grantIndex === index ? { ...grant, ...patch } : grant
      ),
    }));
  };
  const save = async () => {
    const grants = policy.grants
      .filter((grant) => grant.provider.trim() && grant.model.trim())
      .map((grant) => ({
        provider: grant.provider.trim(),
        model: grant.model.trim(),
        ...(grant.group?.trim() ? { group: grant.group.trim() } : {}),
      }));
    if (grants.length === 0) {
      setError(t("native.grantRequired"));
      return;
    }
    setSaving(true);
    setError("");
    try {
      await saveNativePolicy({ ...policy, grants });
      onSaved();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setSaving(false);
    }
  };
  const quota = (field: keyof NativePolicy, label: string) => (
    <label>
      {label}
      <input
        className="input"
        type="number"
        min={0}
        value={String(policy[field] ?? 0)}
        onChange={(event) => setPolicy({ ...policy, [field]: numberValue(event.target.value) })}
      />
    </label>
  );
  return (
    <div className="modal-overlay">
      <div className="modal" style={{ maxWidth: 900 }}>
        <h2>{t("native.editTitle")}</h2>
        <div className="muted mono">{initial.key_hash.slice(0, 24)}…</div>
        <label className="native-check">
          <input
            type="checkbox"
            role="switch"
            checked={policy.enabled}
            onChange={(event) => setPolicy({ ...policy, enabled: event.target.checked })}
          />
          {t("native.enabled")}
        </label>
        <div className="muted">{t("native.upstreamOpaqueHint")}</div>
        {policy.grants.map((grant, index) => (
          <div className="card" key={index} style={{ marginTop: 10 }}>
            <div className="form-grid two">
              <input className="input" list={`native-providers-${index}`} value={grant.provider} placeholder={t("native.provider")} onChange={(e) => updateGrant(index, { provider: e.target.value })} />
              <datalist id={`native-providers-${index}`}>
                <option value="*" />
                {[...new Set(catalog.map((item) => item.provider))].map((provider) => <option value={provider} key={provider} />)}
              </datalist>
              <input className="input" list={`native-models-${index}`} value={grant.model} placeholder={t("native.model")} onChange={(e) => updateGrant(index, { model: e.target.value })} />
              <datalist id={`native-models-${index}`}>
                {[...new Set(catalog.filter((item) => grant.provider === "*" || item.provider === grant.provider).map((item) => item.model))].map((model) => <option value={model} key={model} />)}
              </datalist>
              <input className="input" list={`native-groups-${index}`} value={grant.group ?? ""} placeholder={t("native.group")} onChange={(e) => updateGrant(index, { group: e.target.value || undefined })} />
              <datalist id={`native-groups-${index}`}>
                {[...new Set(catalog.filter((item) => (grant.provider === "*" || item.provider === grant.provider) && (!grant.model || item.model === grant.model)).map((item) => item.group).filter((group): group is string => !!group))].map((group) => <option value={group} key={group} />)}
              </datalist>
            </div>
            <button className="btn sm danger-outline" onClick={() => setPolicy({ ...policy, grants: policy.grants.filter((_, i) => i !== index) })}>{t("keys.delete")}</button>
          </div>
        ))}
        <button className="btn sm" onClick={() => setPolicy({ ...policy, grants: [...policy.grants, emptyGrant()] })}>+ {t("native.addGrant")}</button>
        <div className="form-grid two" style={{ marginTop: 16 }}>
          {quota("rpm", t("native.rpm"))}
          {quota("daily_calls", t("native.dailyCalls"))}
          {quota("weekly_calls", t("native.weeklyCalls"))}
          {quota("daily_tokens", t("native.dailyTokens"))}
          {quota("weekly_tokens", t("native.weeklyTokens"))}
        </div>
        <div className="muted">{t("native.zeroUnlimited")}</div>
        {error && <div className="error">{error}</div>}
        <div className="modal-actions">
          <button className="btn" onClick={onClose} disabled={saving}>{t("mapping.cancel")}</button>
          <button className="btn primary" onClick={() => void save()} disabled={saving}>{t("mapping.save")}</button>
        </div>
      </div>
    </div>
  );
}
