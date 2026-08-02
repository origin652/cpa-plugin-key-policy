import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { KeyPublic } from "../types";

vi.mock("../api/keys", () => ({
  listKeys: vi.fn(),
  patchKey: vi.fn(),
  rotateKey: vi.fn(),
  resetRPM: vi.fn(),
  resetUsage: vi.fn(),
  deleteKey: vi.fn(),
}));

vi.mock("../components/KeyForm", () => ({
  default: () => <div data-testid="key-form" />,
}));

const { translate } = vi.hoisted(() => ({
  translate: (key: string) => key,
}));

vi.mock("../i18n", () => ({
  useT: () => translate,
}));

import { listKeys, resetRPM, resetUsage } from "../api/keys";
import KeyEdit from "./KeyEdit";

const key: KeyPublic = {
  id: "team-a",
  name: "Team A",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  models: [],
  daily_limit_usd: 10,
  weekly_limit_usd: 50,
  usage: {
    daily_usd: 8,
    weekly_usd: 20,
    daily_limit_usd: 10,
    weekly_limit_usd: 50,
  },
};

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("KeyEdit reset menu", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(listKeys).mockResolvedValue([key]);
    vi.stubGlobal("confirm", vi.fn(() => true));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("uses the same daily, weekly, and RPM reset menu as the key list", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/keys/team-a/edit"]}>
          <Routes>
            <Route path="/keys/:id/edit" element={<KeyEdit />} />
          </Routes>
        </MemoryRouter>,
      );
      await tick();
    });

    const details = container.querySelector<HTMLDetailsElement>(".reset-menu");
    expect(details).not.toBeNull();
    expect(details?.querySelector("summary")?.textContent).toContain("keys.reset");
    const buttons = Array.from(details?.querySelectorAll<HTMLButtonElement>("button") ?? []);
    expect(buttons.map((button) => button.textContent)).toEqual([
      "keys.resetDaily",
      "keys.resetWeekly",
      "keys.resetRpm",
    ]);

    await act(async () => {
      buttons[0]?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(confirm).toHaveBeenCalledWith("keys.resetDailyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "daily");

    await act(async () => {
      buttons[2]?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(resetRPM).toHaveBeenCalledWith("team-a");
  });
});
