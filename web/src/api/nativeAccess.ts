import { apiClient, pluginPath } from "./client";
import type { NativeIdentity, NativePolicy } from "../types";

export async function fetchPluginMode(): Promise<"legacy" | "native-access"> {
  const { data } = await apiClient().get<{ mode?: string }>(pluginPath("/status"));
  return data.mode === "native-access" ? "native-access" : "legacy";
}

export async function listNativeIdentities(): Promise<NativeIdentity[]> {
  const { data } = await apiClient().get<{ identities?: NativeIdentity[] }>(
    pluginPath("/identities"),
  );
  return data.identities ?? [];
}

export async function listNativePolicies(): Promise<NativePolicy[]> {
  const { data } = await apiClient().get<{ policies?: NativePolicy[] }>(
    pluginPath("/policies"),
  );
  return data.policies ?? [];
}

export async function saveNativePolicy(policy: NativePolicy): Promise<void> {
  await apiClient().put(pluginPath("/policies"), policy);
}
