import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { setSession, verifySession } from "../store/session";
import { isEmbedded } from "../store/panelAuth";
import { useT } from "../i18n";

export default function Login() {
  const nav = useNavigate();
  const t = useT();
  // Default to the current page origin: when the UI is hosted by CPA at
  // /v0/resource/plugins/cpa-key-policy/index.html, the API is on the same
  // origin, so same-origin requests avoid CORS and hit the right host:port.
  // In standalone dev (vite), origin is the dev server, which the vite proxy
  // forwards to CPA — still correct.
  const [baseUrl, setBaseUrl] = useState(
    typeof window !== "undefined" ? window.location.origin : "http://127.0.0.1:8317",
  );
  const [secretKey, setSecretKey] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    if (!secretKey.trim()) {
      setError(t("login.secretRequired"));
      return;
    }
    setBusy(true);
    try {
      setSession(baseUrl, secretKey);
      await verifySession(fetch);
      nav("/keys");
    } catch (err) {
      setError((err as Error).message || t("login.loginFailed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="app">
      <div className="header">
        <div>
          <h1>{t("header.title")}</h1>
          <div className="sub">{t("login.subTitle")}</div>
        </div>
      </div>
      <form className="card" onSubmit={submit} style={{ maxWidth: 460 }}>
        <div className="form-row">
          <label>{t("login.baseUrl")}</label>
          <input
            className="input"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
            placeholder={t("login.baseUrlPlaceholder")}
            autoFocus
          />
        </div>
        <div className="form-row">
          <label>{t("login.managementKey")}</label>
          <input
            className="input"
            type="password"
            value={secretKey}
            onChange={(e) => setSecretKey(e.target.value)}
            placeholder={t("login.managementKeyPlaceholder")}
          />
        </div>
        {error && <div className="error">{error}</div>}
        <button className="btn primary" type="submit" disabled={busy}>
          {busy ? t("login.verifying") : t("login.submit")}
        </button>
        <div className="muted" style={{ marginTop: 12, fontSize: 12 }}>
          {t("login.memoryNote")}
        </div>
        {isEmbedded() && (
          <div className="muted" style={{ marginTop: 8, fontSize: 12 }}>
            {t("login.embeddedFallback")}
          </div>
        )}
      </form>
    </div>
  );
}
