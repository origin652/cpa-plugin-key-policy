// Mirror the official CPA management panel's selected language onto this
// iframe-hosted plugin UI.
//
// The panel uses i18next + a persisted zustand store keyed `cli-proxy-language`
// (locales `zh-CN | zh-TW | en | ru`), and also sets `document.documentElement.lang`
// on its own document. Because the plugin is a same-origin iframe (see
// themeSync.ts rationale), it is a SEPARATE document and does NOT inherit the
// parent's `lang` attribute. We read the parent's applied `lang` and mirror it
// into our own i18n store (see ./index.ts), with two backstops:
//   1. A StorageEvent listener on `cli-proxy-language` — fires cross-browsing-
//      context on same origin (parent update propagates to child frames).
//   2. A MutationObserver on the parent's <html lang> (immediate, DOM-driven).
//
// When NOT embedded (direct page open, self === top) there is no parent to
// follow; we resolve the initial language from this page's own <html lang>
// (set by us) or fall back to a browser-default heuristic, and never touch
// localStorage — same isolation policy as themeSync. We also do NOT write to
// the shared `cli-proxy-language` key; only the panel writes it.

import { getLocale, isSupportedLocale, setLocale, type Locale } from "./index";

const PANEL_LANG_STORAGE_KEY = "cli-proxy-language";

let observer: MutationObserver | null = null;
let storageHandler: ((e: StorageEvent) => void) | null = null;
let started = false;

function isEmbedded(): boolean {
  try {
    return window.self !== window.top;
  } catch {
    // Cross-origin window.top access throws → not same-origin → not embedded.
    return false;
  }
}

// Parse the panel's `cli-proxy-language` localStorage value. Riesen zustand
// persist envelope shape: `{"state":{"language":"en"},"version":0}`. Legacy raw
// code string (`"en"`) is tolerated too. Returns the supported locale or null.
function parseStoredLanguage(raw: string): Locale | null {
  if (!raw) return null;
  // Try JSON envelope first.
  try {
    const parsed = JSON.parse(raw) as { state?: { language?: unknown }; language?: unknown } | unknown;
    const fromState = (parsed as { state?: { language?: unknown } })?.state?.language;
    const fromTop = (parsed as { language?: unknown })?.language;
    const candidate =
      typeof fromState === "string" ? fromState
        : typeof fromTop === "string" ? fromTop
          : typeof parsed === "string" ? (parsed as string)
            : null;
    if (candidate && isSupportedLocale(candidate)) return candidate;
  } catch {
    // Not JSON — maybe a raw legacy locale code.
    if (isSupportedLocale(raw)) return raw as Locale;
  }
  return null;
}

// Read the parent page's `<html lang>` value (source of truth for what's
// applied). Returns null when there's no readable same-origin parent.
function readParentLang(): string | null {
  if (!isEmbedded()) return null;
  let parentEl: HTMLElement | null;
  try {
    parentEl = window.parent.document.documentElement;
  } catch {
    return null;
  }
  return parentEl.getAttribute("lang");
}

// Read the panel's persisted language from the localStorage it shares with us
// (same-origin). Returns null if absent / unreadable.
function readStoredLang(): Locale | null {
  try {
    const raw = localStorage.getItem(PANEL_LANG_STORAGE_KEY);
    if (!raw) return null;
    return parseStoredLanguage(raw);
  } catch {
    return null;
  }
}

// Resolve the locale we should apply, in priority order. Embedded: prefer the
// parent DOM's applied `lang` (handles the panel covering `auto` resolving to a
// concrete locale on the DOM directly), then the shared localStorage. Standalone:
// prefer this document's own `<html lang>`, then browser default heuristic, then
// zh-CN fallback.
function resolveLocale(): Locale {
  if (isEmbedded()) {
    const parentLang = readParentLang();
    if (parentLang && isSupportedLocale(parentLang)) return parentLang as Locale;
    const stored = readStoredLang();
    if (stored) return stored;
  } else {
    const own = document.documentElement.getAttribute("lang");
    if (own && isSupportedLocale(own)) return own as Locale;
    const nav = typeof navigator !== "undefined" ? navigator.language : "";
    if (nav) {
      const lower = nav.toLowerCase();
      if (lower.startsWith("zh-tw") || lower === "zh-hant") return "zh-TW";
      if (lower.startsWith("zh")) return "zh-CN";
      if (lower.startsWith("ru")) return "ru";
      if (lower.startsWith("en")) return "en";
    }
  }
  // Final fallback — also matches panel fallbackLng = "zh-CN".
  return "zh-CN";
}

// Apply the resolved locale unless it already is current (avoids spurious
// notify emissions that would churn subscribers).
function sync(): void {
  const next = resolveLocale();
  if (next !== getLocale()) setLocale(next);
}

// Begin mirroring the panel's language. Idempotent. Called once from main.tsx
// before React mounts so the first paint is already in the right language.
export function initLangSync(): void {
  if (started) return;
  started = true;

  sync();

  if (!isEmbedded()) return; // standalone: nothing to watch.

  let parentEl: HTMLElement;
  try {
    parentEl = window.parent.document.documentElement;
  } catch {
    // No readable same-origin parent; listen only to storage as a backstop.
    storageHandler = (e: StorageEvent) => {
      if (e.key === PANEL_LANG_STORAGE_KEY) sync();
    };
    window.addEventListener("storage", storageHandler);
    return;
  }

  observer = new MutationObserver(() => sync());
  observer.observe(parentEl, { attributes: true, attributeFilter: ["lang"] });

  storageHandler = (e: StorageEvent) => {
    if (e.key === PANEL_LANG_STORAGE_KEY) sync();
  };
  window.addEventListener("storage", storageHandler);
}

// For tests: tear down listeners + reset state so a new init can run.
export function _teardownLangSync(): void {
  observer?.disconnect();
  observer = null;
  if (storageHandler) {
    window.removeEventListener("storage", storageHandler);
    storageHandler = null;
  }
  started = false;
}

// For tests: expose resolvers so cases can assert without a live parent.
export function _resolveLocale(): Locale {
  return resolveLocale();
}
export function _parseStoredLanguage(raw: string): Locale | null {
  return parseStoredLanguage(raw);
}