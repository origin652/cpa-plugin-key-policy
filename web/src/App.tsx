import { Routes, Route, Navigate, useNavigate, Link, useLocation } from "react-router-dom";
import { useEffect, useState } from "react";
import { isAuthed, subscribe, clearSession, getSession, bootstrapFromPanel } from "./store/session";
import { useT } from "./i18n";
import Login from "./pages/Login";
import KeyList from "./pages/KeyList";
import KeyNew from "./pages/KeyNew";
import KeyEdit from "./pages/KeyEdit";
import KeyUsage from "./pages/KeyUsage";
import ModelPick from "./pages/ModelPick";
import Mapping, { AliasEditForm, RuleEditForm } from "./pages/Mapping";
import NativeAccess from "./pages/NativeAccess";
import { fetchPluginMode } from "./api/nativeAccess";

function useAuthTick() {
  const [, setTick] = useState(0);
  useEffect(() => subscribe(() => setTick((t) => t + 1)), []);
  return isAuthed();
}

// Desktop top horizontal nav. Mirrors the Stitch "Quiet Paper" design: left =
// app title + base url, right = nav links + logout. Mobile keeps the legacy
// .header (hidden on desktop via CSS) and bottom tab bar instead.
function TopNav({ mode }: { mode: "legacy" | "native-access" }) {
  const t = useT();
  const nav = useNavigate();
  const loc = useLocation();
  const s = getSession();
  if (!s) return null;
  // Active state: highlight the nav item matching the current path prefix.
  const onKeys = loc.pathname === "/keys" || loc.pathname.startsWith("/keys/");
  const onNew = loc.pathname === "/keys/new" || loc.pathname.startsWith("/keys/new/");
  const onMapping = loc.pathname === "/mapping" || loc.pathname.startsWith("/mapping/");
  return (
    <div className="topnav">
      <div className="topnav-inner">
        <div className="topnav-brand">
          <span className="tn-title">{t("header.title")}</span>
          <span className="tn-sub">{s.baseUrl}</span>
        </div>
        <div className="topnav-actions">
          <Link to="/keys" className={"tn-link" + (onKeys && !onNew ? " active" : "")}>{t("header.keyList")}</Link>
          {mode === "legacy" && <Link to="/keys/new" className={"tn-link" + (onNew ? " active" : "")}>{t("header.newKey")}</Link>}
          {mode === "legacy" && <Link to="/mapping" className={"tn-link" + (onMapping ? " active" : "")}>{t("header.mapping")}</Link>}
          <button
            className="btn sm"
            onClick={() => { clearSession(); nav("/login"); }}
          >
            {t("header.logout")}
          </button>
        </div>
      </div>
    </div>
  );
}

function Shell() {
  const authed = useAuthTick();
  const [bootstrapped, setBootstrapped] = useState(false);
  const [mode, setMode] = useState<"legacy" | "native-access" | null>(null);
  const t = useT();

  // When not yet authenticated, try once to reuse the panel's saved
  // management key (same-origin iframe embed). Only runs when not authed and
  // not already attempted, so a manual login or a successful bootstrap won't
  // re-trigger it.
  useEffect(() => {
    if (authed || bootstrapped) return;
    let alive = true;
    void bootstrapFromPanel().finally(() => {
      if (alive) setBootstrapped(true);
    });
    return () => {
      alive = false;
    };
  }, [authed, bootstrapped]);

  useEffect(() => {
    if (!authed) {
      setMode(null);
      return;
    }
    void fetchPluginMode().then(setMode).catch(() => setMode("legacy"));
  }, [authed]);

  if (!authed) {
    if (!bootstrapped) {
      return <div className="app muted" style={{ padding: "40px 20px" }}>{t("session.restoring")}</div>;
    }
    return (
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    );
  }
  if (mode === null) {
    return <div className="app muted" style={{ padding: "40px 20px" }}>{t("keys.loading")}</div>;
  }
  return (
    <div className="app">
      <TopNav mode={mode} />
      <Routes>
        <Route path="/keys" element={mode === "native-access" ? <NativeAccess /> : <KeyList />} />
        {mode === "legacy" && <Route path="/keys/new" element={<KeyNew />} />}
        {mode === "legacy" && <Route path="/keys/new/models" element={<ModelPick />} />}
        {mode === "legacy" && <Route path="/keys/:id/edit" element={<KeyEdit />} />}
        {mode === "legacy" && <Route path="/keys/:id/edit/models" element={<ModelPick />} />}
        {mode === "legacy" && <Route path="/mapping/pick-target" element={<ModelPick />} />}
        {mode === "legacy" && <Route path="/keys/:id/usage" element={<KeyUsage />} />}
        {mode === "legacy" && <Route path="/mapping" element={<Mapping />} />}
        {mode === "legacy" && <Route path="/mapping/alias/:aliasName" element={<AliasEditForm />} />}
        {mode === "legacy" && <Route path="/mapping/rule/:ruleName" element={<RuleEditForm />} />}
        <Route path="*" element={<Navigate to="/keys" replace />} />
      </Routes>
    </div>
  );
}

export default function App() {
  return (
    <Routes>
      <Route path="/*" element={<Shell />} />
    </Routes>
  );
}
