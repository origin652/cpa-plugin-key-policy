import React from "react";
import ReactDOM from "react-dom/client";
import { HashRouter } from "react-router-dom";
import App from "./App";
import "./styles.css";
import { initThemeSync } from "./store/themeSync";
import { initLangSync } from "./i18n/langSync";

// Mark iframe embedding so CSS can reserve room for CPA's toolbar.
// The detection ensures the shift is only applied when embedded in the CPA panel.
document.documentElement.classList.toggle("embedded", window.self !== window.top);

// Mirror the host CPA panel's theme (light/white/dark) onto this iframe's
// <html> before React mounts, so the first paint already matches the panel and
// there's no theme flash. No-op / light fallback when opened standalone.
initThemeSync();

// Likewise mirror the panel's selected language (zh-CN/zh-TW/en/ru) into our
// i18n store before React mounts, so the first paint is already in the right
// language. Falls back to browser default / zh-CN when standalone.
initLangSync();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <HashRouter>
      <App />
    </HashRouter>
  </React.StrictMode>,
);
