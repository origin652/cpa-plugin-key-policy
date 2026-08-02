import { useCallback, useEffect, useRef, useState } from "react";
import { resetRPM, resetUsage } from "../api/keys";
import type { UsageResetWindow } from "../api/keys";
import { useT } from "../i18n";

type KeyResetKind = UsageResetWindow | "rpm";

export default function KeyResetMenu({
  keyId,
  onResetComplete,
  onOpenChange,
}: {
  keyId: string;
  onResetComplete?: () => void | Promise<void>;
  onOpenChange?: (open: boolean) => void;
}) {
  const t = useT();
  const menuRef = useRef<HTMLDetailsElement>(null);
  const [open, setOpen] = useState(false);

  const updateOpen = useCallback((next: boolean) => {
    setOpen(next);
    onOpenChange?.(next);
  }, [onOpenChange]);

  const closeMenu = useCallback(() => {
    if (menuRef.current) {
      menuRef.current.open = false;
    }
    updateOpen(false);
  }, [updateOpen]);

  useEffect(() => {
    if (!open) return;

    const onDocumentPointerDown = (event: PointerEvent) => {
      if (!menuRef.current?.contains(event.target as Node)) {
        closeMenu();
      }
    };
    const onDocumentKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      closeMenu();
      menuRef.current?.querySelector<HTMLElement>("summary")?.focus();
    };

    document.addEventListener("pointerdown", onDocumentPointerDown);
    document.addEventListener("keydown", onDocumentKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onDocumentPointerDown);
      document.removeEventListener("keydown", onDocumentKeyDown);
    };
  }, [closeMenu, open]);

  const selectReset = async (kind: KeyResetKind) => {
    closeMenu();
    if (kind === "daily" && !confirm(t("keys.resetDailyConfirm", { id: keyId }))) return;
    if (kind === "weekly" && !confirm(t("keys.resetWeeklyConfirm", { id: keyId }))) return;

    try {
      if (kind === "rpm") {
        await resetRPM(keyId);
      } else {
        await resetUsage(keyId, kind);
      }
      await onResetComplete?.();
    } catch (e) {
      alert((e as Error).message ?? t("keys.resetFailed"));
    }
  };

  return (
    <details
      ref={menuRef}
      className="reset-menu"
      onClick={(e) => e.stopPropagation()}
      onToggle={(e) => updateOpen(e.currentTarget.open)}
    >
      <summary className="btn sm" aria-label={t("keys.reset")}>
        {t("keys.reset")} <span aria-hidden="true">▾</span>
      </summary>
      <div className="reset-menu-options" role="menu">
        <button type="button" role="menuitem" onClick={() => void selectReset("daily")}>{t("keys.resetDaily")}</button>
        <button type="button" role="menuitem" onClick={() => void selectReset("weekly")}>{t("keys.resetWeekly")}</button>
        <button type="button" role="menuitem" onClick={() => void selectReset("rpm")}>{t("keys.resetRpm")}</button>
      </div>
    </details>
  );
}
