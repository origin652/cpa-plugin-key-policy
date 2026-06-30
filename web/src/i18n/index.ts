// Lightweight i18n for this plugin's web UI.
//
// The official CPA management panel uses react-i18next + a persisted language
// store keyed `cli-proxy-language` with locales `zh-CN | zh-TW | en | ru`. We
// do NOT depend on the panel's runtime; we only mirror its *contract* — the
// localStorage key/shape, the supported locale codes, and the parent page's
// `<html lang>` attribute — so this same-origin iframe child can switch
// language when the panel switches. See `langSync.ts` for the sync bridge.
//
// Design notes:
//   - Hand-written store + pub/sub, matching the existing session/themeSync
//     style (no i18n library dependency, smaller single-file bundle).
//   - Message bundles are nested JSON; keys are dot paths ("login.submit").
//   - Interpolation is `{{name}}` placeholder substitution, mirroring i18next.
//   - The "current locale" is module-global; components re-render via useT(),
//     which subscribes to the store (port of session's subscribe pattern).
//
// Safety: when an embedded panel writes `cli-proxy-language`, langSync calls
// setLocale() here, which notifies all subscribed components. When standalone,
// langSync falls back to browser default / zh-CN and never touches storage.

import { useEffect, useState } from "react";
import zhCN from "./locales/zh-CN.json";
import zhTW from "./locales/zh-TW.json";
import en from "./locales/en.json";
import ru from "./locales/ru.json";

export type Locale = "zh-CN" | "zh-TW" | "en" | "ru";

// i18next-style fallback chain: requested locale → zh-CN (default base).
const FALLBACK: Locale = "zh-CN";

const MESSAGES: Record<Locale, Record<string, unknown>> = {
  "zh-CN": zhCN as Record<string, unknown>,
  "zh-TW": zhTW as Record<string, unknown>,
  en: en as Record<string, unknown>,
  ru: ru as Record<string, unknown>,
};

const SUPPORTED: Locale[] = ["zh-CN", "zh-TW", "en", "ru"];

export function isSupportedLocale(x: string): x is Locale {
  return (SUPPORTED as string[]).includes(x);
}

let current: Locale = FALLBACK;
const listeners = new Set<() => void>();

function emit(): void {
  for (const fn of listeners) fn();
}

export function getLocale(): Locale {
  return current;
}

export function setLocale(loc: Locale): void {
  if (!isSupportedLocale(loc) || loc === current) return;
  current = loc;
  emit();
}

export function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

// Resolve a dot-path key against a nested message object. Returns undefined on
// any miss so the caller can fall back to the key itself (i18next `returnNull`
// /render-the-key behavior). Traverses the requested locale, then the fallback.
function lookup(loc: Locale, key: string): string | undefined {
  const messages = MESSAGES[loc];
  const parts = key.split(".");
  let node: unknown = messages;
  for (const p of parts) {
    if (node && typeof node === "object" && p in (node as Record<string, unknown>)) {
      node = (node as Record<string, unknown>)[p];
    } else {
      node = undefined;
      break;
    }
  }
  if (typeof node === "string") return node;
  return undefined;
}

// Interpolate `{{name}}` placeholders from `vars`. Unknown placeholders are
// left as-is (visible to the developer) rather than silently emptied.
function interpolate(template: string, vars?: Record<string, string | number>): string {
  if (!vars) return template;
  return template.replace(/\{\{(\w+)\}\}/g, (m, name: string) => {
    const v = vars[name];
    return v === undefined || v === null ? m : String(v);
  });
}

// A safe t(): resolves against current locale, then FALLBACK, then returns the
// key itself so missing translations never show empty UI.
export function translate(key: string, vars?: Record<string, string | number>): string {
  const primary = lookup(getLocale(), key);
  const fallback = lookup(FALLBACK, key);
  const template = primary ?? fallback ?? key;
  return interpolate(template, vars);
}

// React hook — subscribes to locale changes and re-renders on switch.
// Returns a memo-stable `t` bound to the current render's locale.
export function useT() {
  const [, setTick] = useState(0);
  useEffect(() => subscribe(() => setTick((t) => (t + 1) % 1_000_000)), []);
  return translate;
}

// For tests / external setup: reset to a known locale without touching the
// panel bridge. Initialize the default — when no panel feeds us a locale,
// langSync will resolve from <html lang> / browser and call setLocale().
export function _resetLocale(loc: Locale = FALLBACK): void {
  current = loc;
  emit();
}