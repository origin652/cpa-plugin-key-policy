import { useMemo, useState } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { createKey } from "../api/keys";
import KeyForm from "../components/KeyForm";
import PlainKeyModal from "../components/PlainKeyModal";
import { MobileFormHeader, MobileTabBar } from "./KeyList";
import { useT } from "../i18n";
import type { KeyPublic, ModelRule } from "../types";

export default function KeyNew() {
  const nav = useNavigate();
  const loc = useLocation();
  const t = useT();
  const [plain, setPlain] = useState<string | null>(null);

  const title = t("new.title");

  // When the standalone model-picker page returns here with a selection,
  // merge it into the form's initial models. Pricing rows for newly-picked
  // aliases start at 0; preserved aliases keep their existing rows via
  // KeyForm's price-map init from `initial.models`.
  const picked = (loc.state as { pickedModels?: ModelRule[] } | null)?.pickedModels;
  const initial = useMemo<KeyPublic | undefined>(
    () => (picked ? ({ id: "", name: "", enabled: true, rpm: 0, models: picked, daily_limit_usd: 0, weekly_limit_usd: 0 } as KeyPublic) : undefined),
    [picked],
  );

  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden">
        <h1>{title}</h1>
      </div>
      <MobileFormHeader title={title} backTo="/keys" />
      <KeyForm
        initial={initial}
        pickPath="/keys/new/models"
        submitLabel={t("new.create")}
        onCancel={() => nav("/keys")}
        onSubmit={async (v) => {
          const r = await createKey({
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
          if (r.plain_key) setPlain(r.plain_key);
        }}
      />
      <p className="fp-note mobile-hidden">{t("login.memoryNote")}</p>
      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={t("plainModal.created")}
          onClose={() => {
            setPlain(null);
            nav("/keys");
          }}
        />
      )}
      <MobileTabBar active="new" />
    </div>
  );
}
