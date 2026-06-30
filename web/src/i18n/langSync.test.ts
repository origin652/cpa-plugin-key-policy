import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  initLangSync,
  _resolveLocale,
  _parseStoredLanguage,
  _teardownLangSync,
} from "./langSync";
import { getLocale, setLocale, _resetLocale } from "./index";

// langSync reads window.parent.document.documentElement[lang] and the shared
// `cli-proxy-language` localStorage. jsdom gives us a real document but no
// separate parent. We stub window.parent.document with a detached element and
// toggle window.self/top for embedded state — same harness as themeSync.test.

const realSelf = window.self;
const realTop = window.top;
let realParentDescriptor: PropertyDescriptor | undefined;

beforeEach(() => {
  document.documentElement.removeAttribute("lang");
  localStorage.removeItem("cli-proxy-language");
  _resetLocale("zh-CN");
});

afterEach(() => {
  _teardownLangSync();
  Object.defineProperty(window, "self", { value: realSelf, configurable: true });
  Object.defineProperty(window, "top", { value: realTop, configurable: true });
  if (realParentDescriptor) {
    Object.defineProperty(window, "parent", realParentDescriptor);
  }
  localStorage.removeItem("cli-proxy-language");
});

function setEmbedded(embedded: boolean, parentHtml: HTMLElement) {
  Object.defineProperty(window, "self", { value: window, configurable: true });
  Object.defineProperty(window, "top", {
    value: embedded ? ({} as Window) : window,
    configurable: true,
  });
  realParentDescriptor = Object.getOwnPropertyDescriptor(window, "parent");
  Object.defineProperty(window, "parent", {
    configurable: true,
    get: () => ({ document: { documentElement: parentHtml } }),
  });
}

describe("parseStoredLanguage", () => {
  it("parses the zustand persist envelope", () => {
    const raw = JSON.stringify({ state: { language: "en" }, version: 0 });
    expect(_parseStoredLanguage(raw)).toBe("en");
  });

  it("parses a zh-TW envelope", () => {
    const raw = JSON.stringify({ state: { language: "zh-TW" }, version: 0 });
    expect(_parseStoredLanguage(raw)).toBe("zh-TW");
  });

  it("tolerates a legacy bare locale string", () => {
    expect(_parseStoredLanguage("ru")).toBe("ru");
  });

  it("returns null for unsupported codes", () => {
    expect(_parseStoredLanguage("ja")).toBeNull();
    expect(_parseStoredLanguage(JSON.stringify({ state: { language: "fr" } }))).toBeNull();
  });

  it("returns null for empty / garbage", () => {
    expect(_parseStoredLanguage("")).toBeNull();
    expect(_parseStoredLanguage("not json and not a code")).toBeNull();
  });
});

describe("resolveLocale (embedded)", () => {
  it("prefers parent <html lang> over storage", () => {
    const parentHtml = document.createElement("html");
    parentHtml.setAttribute("lang", "ru");
    setEmbedded(true, parentHtml);
    localStorage.setItem("cli-proxy-language", JSON.stringify({ state: { language: "en" }, version: 0 }));
    expect(_resolveLocale()).toBe("ru");
  });

  it("falls back to stored language when parent has no lang attr", () => {
    const parentHtml = document.createElement("html");
    setEmbedded(true, parentHtml);
    localStorage.setItem("cli-proxy-language", JSON.stringify({ state: { language: "zh-TW" }, version: 0 }));
    expect(_resolveLocale()).toBe("zh-TW");
  });

  it("falls back to zh-CN when neither parent lang nor storage is usable", () => {
    const parentHtml = document.createElement("html");
    setEmbedded(true, parentHtml);
    expect(_resolveLocale()).toBe("zh-CN");
  });
});

describe("resolveLocale (standalone)", () => {
  const realNavigator = navigator;
  let stubbedNav: Navigator;

  beforeEach(() => {
    // jsdom defaults navigator.language to "en-US", which would hit the
    // browser-language heuristic. Stub a no-language navigator so the zh-CN
    // fallback path is exercised unambiguously.
    stubbedNav = { ...realNavigator, language: "" } as Navigator;
    Object.defineProperty(window, "navigator", { value: stubbedNav, configurable: true });
  });
  afterEach(() => {
    Object.defineProperty(window, "navigator", { value: realNavigator, configurable: true });
  });

  it("uses this document's own <html lang> if set", () => {
    setEmbedded(false, document.documentElement);
    document.documentElement.setAttribute("lang", "en");
    expect(_resolveLocale()).toBe("en");
  });

  it("uses the browser-language heuristic when no <html lang> is set", () => {
    Object.defineProperty(window, "navigator", {
      value: { ...realNavigator, language: "zh-CN" } as Navigator,
      configurable: true,
    });
    setEmbedded(false, document.documentElement);
    expect(_resolveLocale()).toBe("zh-CN");
  });

  it("falls back to zh-CN when nothing is set", () => {
    setEmbedded(false, document.documentElement);
    expect(_resolveLocale()).toBe("zh-CN");
  });
});

describe("initLangSync apply", () => {
  it("applies parent lang to self store on init (embedded)", () => {
    const parentHtml = document.createElement("html");
    parentHtml.setAttribute("lang", "ru");
    setEmbedded(true, parentHtml);
    initLangSync();
    expect(getLocale()).toBe("ru");
  });

  it("does NOT follow parent when standalone (uses own lang or fallback)", () => {
    // jsdom navigator.language is "en-US"; neutralize it so we hit the zh-CN
    // fallback (the standalone path uses own <html lang> or browser default).
    const realNav = navigator;
    Object.defineProperty(window, "navigator", {
      value: { ...realNav, language: "" } as Navigator,
      configurable: true,
    });
    setEmbedded(false, document.documentElement);
    initLangSync();
    expect(getLocale()).toBe("zh-CN");
    Object.defineProperty(window, "navigator", { value: realNav, configurable: true });
  });

  it("reacts to parent <html lang> changes via MutationObserver", async () => {
    const parentHtml = document.createElement("html");
    parentHtml.setAttribute("lang", "en");
    setEmbedded(true, parentHtml);
    initLangSync();
    expect(getLocale()).toBe("en");

    parentHtml.setAttribute("lang", "zh-TW");
    await new Promise((r) => setTimeout(r, 0));
    expect(getLocale()).toBe("zh-TW");

    parentHtml.setAttribute("lang", "ru");
    await new Promise((r) => setTimeout(r, 0));
    expect(getLocale()).toBe("ru");
  });

  it("reacts to a storage event on cli-proxy-language", () => {
    const parentHtml = document.createElement("html");
    // Parent DOM says en; a storage event then claims zh-TW was written AND
    // the DOM is updated to match (panel re-applies on storage write).
    parentHtml.setAttribute("lang", "en");
    setEmbedded(true, parentHtml);
    initLangSync();
    expect(getLocale()).toBe("en");

    parentHtml.setAttribute("lang", "zh-TW");
    window.dispatchEvent(new StorageEvent("storage", { key: "cli-proxy-language" }));
    expect(getLocale()).toBe("zh-TW");
  });

  it("ignores storage events for other keys", () => {
    const parentHtml = document.createElement("html");
    parentHtml.setAttribute("lang", "en");
    setEmbedded(true, parentHtml);
    initLangSync();
    parentHtml.setAttribute("lang", "ru"); // would change to ru if it reacted
    window.dispatchEvent(new StorageEvent("storage", { key: "cli-proxy-theme" }));
    expect(getLocale()).toBe("en");
  });

  it("is idempotent — calling twice does not double-register", () => {
    const parentHtml = document.createElement("html");
    parentHtml.setAttribute("lang", "ru");
    setEmbedded(true, parentHtml);
    initLangSync();
    initLangSync();
    expect(getLocale()).toBe("ru");
  });
});

describe("setLocale interplay", () => {
  it("does not churn when already at the resolved locale", () => {
    const parentHtml = document.createElement("html");
    parentHtml.setAttribute("lang", "en");
    setEmbedded(true, parentHtml);
    initLangSync();
    expect(getLocale()).toBe("en");
    // Manually flip away, then a sync() path (via storage event with same DOM)
    // should not revert if DOM still says en.
    setLocale("zh-CN");
    expect(getLocale()).toBe("zh-CN");
    window.dispatchEvent(new StorageEvent("storage", { key: "cli-proxy-language" }));
    expect(getLocale()).toBe("en"); // DOM parent lang wins → back to en
  });
});