import { useEffect, useMemo, useState } from "react";
import { useNavigate, useParams, useLocation } from "react-router-dom";
import { listKeys, patchKey, rotateKey, resetRPM, deleteKey } from "../api/keys";
import type { KeyPublic, ModelRule } from "../types";
import KeyForm from "../components/KeyForm";
import PlainKeyModal from "../components/PlainKeyModal";
import { MobileFormHeader, MobileTabBar } from "./KeyList";
import { useT } from "../i18n";

export default function KeyEdit() {
  const { id } = useParams<{ id: string }>();
  const nav = useNavigate();
  const loc = useLocation();
  const t = useT();
  const [key, setKey] = useState<KeyPublic | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [plain, setPlain] = useState<string | null>(null);
  const [plainTitle, setPlainTitle] = useState("");

  useEffect(() => {
    (async () => {
      setLoading(true);
      try {
        const all = await listKeys();
        const found = all.find((k) => k.id === decodeURIComponent(id ?? ""));
        if (!found) setError(t("keys.notFound"));
        else setKey(found);
      } catch (e) {
        setError((e as Error).message ?? t("keys.loadFailed"));
      } finally {
        setLoading(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  // When the model-picker page returns, merge its selection into the loaded
  // key's models, preserving everything else (id/name/limits/prices). The
  // KeyForm price-map init keeps existing rows for aliases that survived.
  const picked = (loc.state as { pickedModels?: ModelRule[] } | null)?.pickedModels;
  const initial = useMemo<KeyPublic | null>(() => {
    if (!key) return null;
    if (!picked) return key;
    return { ...key, models: picked };
  }, [key, picked]);

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  if (error || !key) return <div className="error">{error || t("edit.notFound")}</div>;
  if (!initial) return null;

  const title = t("edit.title", { id: key.id });

  const onRotate = async () => {
    if (!confirm(t("keys.rotateConfirm", { id: key.id }))) return;
    try {
      const r = await rotateKey(key.id);
      setPlain(r.plain_key);
      setPlainTitle(t("keys.rotated"));
    } catch (e) {
      alert((e as Error).message ?? t("keys.rotateFailed"));
    }
  };
  const onReset = async () => {
    try {
      await resetRPM(key.id);
    } catch (e) {
      alert((e as Error).message ?? t("keys.resetFailed"));
    }
  };
  const onDelete = async () => {
    if (!confirm(t("keys.deleteConfirm", { id: key.id }))) return;
    try {
      await deleteKey(key.id);
      nav("/keys");
    } catch (e) {
      alert((e as Error).message ?? t("keys.deleteFailed"));
    }
  };

  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden">
        <h1>{t("edit.hTitle")}</h1>
        <div className="fp-actions">
          <button className="btn sm" onClick={onReset}>{t("keys.resetRpm")}</button>
          <button className="btn sm" onClick={onRotate}>{t("keys.rotate")}</button>
          <button className="btn sm" onClick={() => nav("/keys")}>{t("keyForm.cancel")}</button>
        </div>
      </div>
      <div className="fp-idline mobile-hidden">
        {key.id}<span className="fp-name">{key.name}</span>
      </div>
      <MobileFormHeader title={title} backTo="/keys" />
      <KeyForm
        initial={initial}
        idReadOnly
        pickPath={`/keys/${encodeURIComponent(key.id)}/edit/models`}
        submitLabel={t("edit.save")}
        onCancel={() => nav("/keys")}
        dangerLabel={t("keys.delete")}
        onDanger={onDelete}
        onSubmit={async (v) => {
          await patchKey({
            id: v.id,
            name: v.name || undefined,
            enabled: v.enabled,
            rpm: v.rpm,
            models: v.models,
            daily_limit_usd: v.daily_limit_usd,
            weekly_limit_usd: v.weekly_limit_usd,
            allow_models_endpoint: v.allow_models_endpoint,
            account_binding: v.account_binding,
            clear_account_binding: v.clear_account_binding,
          });
          nav("/keys");
        }}
      />
      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={plainTitle}
          onClose={() => setPlain(null)}
        />
      )}
      <MobileTabBar active="keys" />
    </div>
  );
}
