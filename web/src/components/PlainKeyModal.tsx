import { useT } from "../i18n";

interface Props {
  plainKey: string;
  title?: string;
  onClose: () => void;
}

// Shows a freshly-issued/rotated plain key once. After closing it can never be retrieved again.
export default function PlainKeyModal({ plainKey, title, onClose }: Props) {
  const t = useT();
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(plainKey);
    } catch {
      /* clipboard may be blocked; user can select manually */
    }
  };
  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>{title ?? t("plainModal.defaultTitle")}</h3>
        <div className="error" style={{ fontWeight: 600 }}>
          {t("plainModal.warning")}
        </div>
        <div className="keybox">{plainKey}</div>
        <div className="actions">
          <button className="btn primary" onClick={copy}>{t("plainModal.copy")}</button>
          <button className="btn" onClick={onClose}>{t("plainModal.saved")}</button>
        </div>
      </div>
    </div>
  );
}
