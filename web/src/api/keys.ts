import { apiClient, pluginPath } from "./client";
import type {
  KeyPublic,
  KeyWriteRequest,
  CreateKeyResponse,
  RotateKeyResponse,
  KeyUsageResponse,
} from "../types";

export async function listKeys(): Promise<KeyPublic[]> {
  const c = apiClient();
  const { data } = await c.get<{ keys: KeyPublic[] }>(pluginPath("/keys"));
  return data.keys ?? [];
}

export async function createKey(
  req: KeyWriteRequest,
): Promise<CreateKeyResponse> {
  const c = apiClient();
  const { data } = await c.post<CreateKeyResponse>(pluginPath("/keys"), req);
  return data;
}

export async function patchKey(
  req: KeyWriteRequest,
): Promise<KeyPublic> {
  const c = apiClient();
  const { data } = await c.patch<{ key: KeyPublic }>(pluginPath("/keys"), req);
  return data.key;
}

export async function deleteKey(id: string): Promise<void> {
  const c = apiClient();
  await c.delete(pluginPath("/keys"), { params: { id } });
}

export async function rotateKey(id: string): Promise<RotateKeyResponse> {
  const c = apiClient();
  const { data } = await c.post<RotateKeyResponse>(
    pluginPath("/keys/rotate"),
    { id },
  );
  return data;
}

export async function resetRPM(id: string): Promise<void> {
  const c = apiClient();
  await c.post(pluginPath("/keys/reset-rpm"), { id });
}

// fetchKeyUsage returns the per-alias usage breakdown for one key (the key
// detail subpage data source). id goes through the query string, matching the
// rotate/reset-rpm/delete convention.
export async function fetchKeyUsage(id: string): Promise<KeyUsageResponse> {
  const c = apiClient();
  const { data } = await c.get<KeyUsageResponse>(pluginPath("/keys/usage"), {
    params: { id },
  });
  return data;
}

// Build ModelRule[] from selected catalog models with alias = target_model.
export function buildModelRules(
  selected: { provider: string; model: string }[],
): { alias: string; provider: string; target_model: string }[] {
  const seen = new Set<string>();
  const out: { alias: string; provider: string; target_model: string }[] = [];
  for (const s of selected) {
    const provider = s.provider.toLowerCase().trim();
    const target = s.model.trim();
    if (!provider || !target) continue;
    const key = provider + "/" + target.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ alias: target, provider, target_model: target });
  }
  return out;
}
