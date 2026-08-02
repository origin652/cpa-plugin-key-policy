import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import type { KeyPublic } from "../types";

vi.mock("../api/keys", () => ({
  listKeys: vi.fn(),
  deleteKey: vi.fn(),
  rotateKey: vi.fn(),
  resetRPM: vi.fn(),
  resetUsage: vi.fn(),
}));

const { translate } = vi.hoisted(() => ({
  translate: (key: string) => key,
}));

vi.mock("../i18n", () => ({
  useT: () => translate,
}));

import KeyList from "./KeyList";
import { listKeys, resetRPM, resetUsage } from "../api/keys";

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

describe("KeyList reset menu", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    (listKeys as ReturnType<typeof vi.fn>).mockResolvedValue([key]);
    vi.stubGlobal("confirm", vi.fn(() => true));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  const resetButton = (label: string) =>
    Array.from(container.querySelectorAll<HTMLButtonElement>(".reset-menu button")).find(
      (button) => button.textContent === label,
    );

  const openResetMenu = async () => {
    const details = container.querySelector<HTMLDetailsElement>(".reset-menu");
    if (!details) throw new Error("reset menu not found");
    await act(async () => {
      details.open = true;
      details.dispatchEvent(new Event("toggle"));
      await tick();
    });
    return details;
  };

  it("dispatches daily, weekly, and RPM resets from one menu", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyList />
        </MemoryRouter>,
      );
      await tick();
    });

    const details = container.querySelector<HTMLDetailsElement>(".reset-menu");
    expect(details).not.toBeNull();
    expect(details?.textContent).toContain("keys.resetDaily");
    expect(details?.textContent).toContain("keys.resetWeekly");
    expect(details?.textContent).toContain("keys.resetRpm");

    await act(async () => {
      resetButton("keys.resetDaily")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });

    expect(confirm).toHaveBeenCalledWith("keys.resetDailyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "daily");

    await act(async () => {
      resetButton("keys.resetWeekly")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(confirm).toHaveBeenCalledWith("keys.resetWeeklyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "weekly");

    await act(async () => {
      resetButton("keys.resetRpm")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(resetRPM).toHaveBeenCalledWith("team-a");
    expect(confirm).toHaveBeenCalledTimes(2);
  });

  it("does not reset a usage window when confirmation is cancelled", async () => {
    vi.mocked(confirm).mockReturnValue(false);
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyList />
        </MemoryRouter>,
      );
      await tick();
    });

    await act(async () => {
      resetButton("keys.resetWeekly")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });

    expect(confirm).toHaveBeenCalledWith("keys.resetWeeklyConfirm");
    expect(resetUsage).not.toHaveBeenCalled();
  });

  it("closes the reset menu on an outside pointer press", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyList />
        </MemoryRouter>,
      );
      await tick();
    });

    const details = await openResetMenu();
    expect(details.open).toBe(true);
    expect(details.closest(".keycard")?.classList.contains("reset-open")).toBe(true);

    await act(async () => {
      document.body.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      await tick();
    });

    expect(details.open).toBe(false);
    expect(details.closest(".keycard")?.classList.contains("reset-open")).toBe(false);
  });

  it("closes the reset menu on Escape", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyList />
        </MemoryRouter>,
      );
      await tick();
    });

    const details = await openResetMenu();
    await act(async () => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      await tick();
    });

    expect(details.open).toBe(false);
    expect(document.activeElement).toBe(details.querySelector("summary"));
  });
});
