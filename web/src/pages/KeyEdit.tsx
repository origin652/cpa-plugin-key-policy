import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { listKeys, patchKey } from "../api/keys";
import type { KeyPublic } from "../types";
import KeyForm from "../components/KeyForm";
import { useT } from "../i18n";

export default function KeyEdit() {
  const { id } = useParams<{ id: string }>();
  const nav = useNavigate();
  const t = useT();
  const [key, setKey] = useState<KeyPublic | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

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

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  if (error || !key) return <div className="error">{error || t("edit.notFound")}</div>;

  return (
    <div>
      <h2 style={{ marginTop: 0 }}>{t("edit.title", { id: key.id })}</h2>
      <KeyForm
        initial={key}
        idReadOnly
        submitLabel={t("edit.save")}
        onCancel={() => nav("/keys")}
        onSubmit={async (v) => {
          await patchKey({
            id: v.id,
            name: v.name || undefined,
            enabled: v.enabled,
            rpm: v.rpm,
            models: v.models,
            daily_limit_usd: v.daily_limit_usd,
            weekly_limit_usd: v.weekly_limit_usd,
          });
          nav("/keys");
        }}
      />
    </div>
  );
}
