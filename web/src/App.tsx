import { Routes, Route, Navigate, useNavigate, Link } from "react-router-dom";
import { useEffect, useState } from "react";
import { isAuthed, subscribe, clearSession, getSession, bootstrapFromPanel } from "./store/session";
import { useT } from "./i18n";
import Login from "./pages/Login";
import KeyList from "./pages/KeyList";
import KeyNew from "./pages/KeyNew";
import KeyEdit from "./pages/KeyEdit";

function useAuthTick() {
  const [, setTick] = useState(0);
  useEffect(() => subscribe(() => setTick((t) => t + 1)), []);
  return isAuthed();
}

function Shell() {
  const authed = useAuthTick();
  const nav = useNavigate();
  const [bootstrapped, setBootstrapped] = useState(false);
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
  const s = getSession()!;
  return (
    <div className="app">
      <div className="header">
        <div>
          <h1>{t("header.title")}</h1>
          <div className="sub">{s.baseUrl}</div>
        </div>
        <div className="actions">
          <Link to="/keys"><button className="btn sm">{t("header.keyList")}</button></Link>
          <Link to="/keys/new"><button className="btn sm primary">{t("header.newKey")}</button></Link>
          <button
            className="btn sm"
            onClick={() => {
              clearSession();
              nav("/login");
            }}
          >
            {t("header.logout")}
          </button>
        </div>
      </div>
      <Routes>
        <Route path="/keys" element={<KeyList />} />
        <Route path="/keys/new" element={<KeyNew />} />
        <Route path="/keys/:id/edit" element={<KeyEdit />} />
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
