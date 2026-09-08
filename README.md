# cpa-key-policy

Downstream **API key policy** plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

In plain words: you issue your own `cpa_…` keys to clients. Each key only sees the models you allow, can be rate-limited and budget-limited, and is routed to real CPA upstream providers (Codex, Claude, OpenAI-compat channels, etc.). CPA’s own `api-keys` can still exist for admin use — **do not put plugin-issued keys into `api-keys`**, or you bypass this plugin.

| | |
|---|---|
| **Repo** | [origin652/cpa-plugin-key-policy](https://github.com/origin652/cpa-plugin-key-policy) |
| **License** | MIT |
| **Install** | [CLIProxyAPI Plugins Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store) or build from source |
| **中文说明** | [README.zh-CN.md](./README.zh-CN.md) |

---

## What it does (human version)

1. **Issue keys** — create many downstream keys; each has an allow-list of models (or shared aliases).
2. **Route** — client calls with alias name `fast`; plugin rewrites to e.g. `codex` + `gpt-5.4-mini`.
3. **Limit** — per-key RPM, optional daily/weekly USD caps, token or per-call billing.
4. **Isolate credentials (tiers / groups)** — pin a request to Codex free/team/… or to a **custom classify group** so it never lands on the wrong auth file.
5. **Multi-target aliases** — one alias can point at several backends (priority or round-robin).
6. **Web UI** — manage keys, global aliases, and credential classification inside CPA.

---

## Concepts

### Downstream key

A plugin-owned secret (`cpa_…`). Authenticated only by this plugin. Holds:

- allowed **models** and/or **aliases**
- RPM
- optional daily / weekly dollar limits
- optional `allow_models_endpoint` (see below)

### Alias (global mapping table)

A reusable name like `fast` that expands to one or more **targets**:

| Field | Meaning |
|--------|---------|
| `provider` | CPA provider id (`codex`, `claude`, or an openai-compatibility **name** such as `cerebras`) |
| `target_model` | Real upstream model id |
| `group` | Optional credential filter (see [Credential groups](#credential-groups-tiers--classify)) |
| `dispatch` | `priority` (always first usable target) or `round-robin` |
| billing | `tokens` (per-million prices) or `per_call` (fixed USD) |

Keys can **reference** aliases instead of duplicating targets. Multi-target aliases expand to several rules with the same alias name; auth and routing share one pick per request so the `group` filter matches the chosen target.

### Credential groups (tiers + classify)

Two sources of “which auth file may serve this request”:

| Kind | How it appears in the picker | Stored in mapping as |
|------|------------------------------|----------------------|
| **Built-in tier** (Codex `plan_type`, Antigravity `tier`) | e.g. Free tier / Team | bare name: `free`, `team`, `supported` |
| **Custom classify rule** | e.g. **Custom · vip** | prefixed: `classify:vip` |

**Runtime rule:** if a mapping sets a group, the plugin scheduler **only** picks auth files in that group. No match → hard failure (`auth_not_found`), never silently fall back to another tier.

**Scheduling:** constraints are applied first. The plugin then keeps the highest available `Priority` tier and selects within that intersection:

- Weight is read from the CPA credential candidate's `Weight`, `Attributes.weight`, or `Metadata.weight` field.
- Missing or invalid weight defaults to `1`; non-positive weight stops new requests; values are capped at `1000000`.
- Scheduler state is scoped by downstream key + provider + model + group + Priority. Keys with different bindings do not share a cursor.
- Lower-priority credentials participate only when every higher-priority credential is unavailable or has non-positive weight.
- When CPA omits frontend-auth metadata, the plugin recovers the unique group from the Header key, `requested_model`, and final provider/model. Missing or ambiguous target information fails; it never falls back to the full pool.
- `global_weighted_round_robin: true` preserves the old group-ignoring behavior only for keys without `account_binding`. It never bypasses an explicit binding or its target group.

### Fail-closed account binding

Each key may define `account_binding.allow`, a case-sensitive list of Go `path.Match` globs over CPA auth candidate IDs. A present empty list means “allow no accounts”; it is not the same as omitting the binding.

```yaml
account_binding:
  allow: ["codex-team-*.json", "openai-compatibility:corp:*"]
  strategy: weighted-round-robin # weighted-round-robin | round-robin | fill-first
```

The scheduler intersects the binding, the uniquely recovered target group, candidate provider/status, positive weight, and highest available Priority. An empty intersection returns `auth_not_bound`; it never returns `Handled:false` or delegates to CPA's global pool. Account-bound requests must present the configured key in a Header. Query-only and conflicting protected credentials are terminated before upstream execution.

CPA-native keys are untouched unless explicitly imported as `native: true`. Importing stores the ordinary key hash plus CPA's irreversible `caller_scope`, never another plaintext copy; the secret is not echoed by the API. Native keys keep host authentication and model routing, but an imported binding is enforced by this scheduler. Host session affinity remains available only to native keys that are not taken over by a binding; plugin-handled RR/WRR does not automatically inherit it.

**Operational boundary:** this is a plugin-only control. Keep the plugin enabled and healthy, and do not use CPA Home mode for account-bound traffic because Home selects before the ordinary plugin scheduler. If the plugin is unloaded/fused, a key that still exists in CPA `api-keys` is again governed only by CPA's global pool. For the strongest fail-closed behavior under plugin removal, use plugin-issued keys and never duplicate them in CPA `api-keys`.

**Custom classification** (Web UI → Mapping → Credential Classification):

- Match auth-file fields (`filename`, `provider`, `plan_type`, `tier`, …) with a regex.
- Assign a **group name** you choose (stored bare on the rule).
- Catalog and mappings use `classify:<name>` so it never collides with built-in `free` / `team`.
- One file can match **multiple** custom groups (shown under each).
- If no custom rule matches → built-in tier (for Codex/Antigravity) or flat (no group) for other auth-file providers.
- OpenAI-compat / API-key channels stay **flat** (no groups).

Configure classify rules in the UI, or via management API (`/classify-rules`, `/classify-preview`, `/catalog`). You do not need to hand-edit state JSON for normal use.

### OpenAI-compatibility providers

Channels under CPA `openai-compatibility` (e.g. a named proxy) use the **channel name** as `provider`. The plugin maps it to CPA’s internal key `openai-compatible-<name>` when routing. Models must be listed on that channel in CPA config, or the host reports no auth for that model.

---

## Capabilities (plugin hooks)

| Hook | Role |
|------|------|
| Frontend auth | Know plugin keys; enforce alias allow-list, RPM, budget; stamp route + group metadata |
| Model router | Alias → provider + target model |
| Request interceptor | Reject query-only or conflicting credentials for account-bound keys before upstream execution |
| Scheduler | Intersect key binding + target group, fail closed, then apply WRR/RR/fill-first within the highest Priority tier |
| Response interceptor | Non-stream JSON: rewrite top-level `model` back to the alias |
| Usage | Token / per-call billing into the state file |
| Management API + embedded Web UI | Keys, aliases, classify rules, status |

---

## Build

Linux `.so` needs cgo and a matching toolchain:

```bash
make test
make build-linux          # builds web UI, then linux amd64/arm64 .so
# or
make web-build
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -buildvcs=false -tags cshared \
  -buildmode=c-shared -o dist/cpa-key-policy_linux_amd64.so ./cmd/cpa-key-policy
```

On Windows, build the `.so` via WSL/Linux. `go test ./...` uses a non-cgo stub so unit tests run without a shared-library toolchain.

Copy the `.so` into CPA `plugins.dir` and enable the plugin in config.

---

## Config

Minimal shape (see also [`config.example.yaml`](./config.example.yaml)):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-key-policy:
      enabled: true
      priority: 10
      state_file: "cpa-key-policy-state.json"
      global_weighted_round_robin: true
```

Notes:

- If `state_file` exists, it is the source of truth for keys / aliases / classify rules / usage.
- `global_weighted_round_robin: true` ignores the selected alias target group only for unbound plugin keys. An explicit account binding always remains restrictive. The default is `false`.
- Prefer creating keys and aliases in the **Web UI** or Management API; seed YAML `keys` is mainly for first boot.
- Never commit real key hashes, management secrets, or live host URLs into public docs.

---

## Web Management UI

Embedded in the plugin. After load, open:

```text
http://<your-cpa-host>:<api-port>/v0/resource/plugins/cpa-key-policy/index.html
```

Login with CPA **management** secret (`remote-management.secret-key` / management password). The secret stays in memory only (not `localStorage`); refresh → re-login.

UI areas:

| Tab / page | Use for |
|------------|---------|
| Keys | Create / edit / rotate / delete keys; bind models or aliases; RPM & budgets |
| Mapping → Aliases | Global multi-target aliases, dispatch, pricing |
| Mapping → Classification | Custom credential groups + match preview |
| Model picker | Catalog of providers; tier / **Custom · …** subgroups |

Dev UI without rebuilding the `.so`:

```bash
cd web
npm install
VITE_CPA_BASE=http://127.0.0.1:8317 npm run dev
```

---

## Management API (summary)

Exact paths (no path templates). Auth: CPA management bearer token.

**Keys**

- `GET/POST/PATCH/DELETE …/keys` (`id` in query or body for mutate)
- `POST …/keys/rotate?id=…`
- `POST …/keys/reset-rpm?id=…`
- `GET …/keys/usage?id=…`
- `GET …/status`

**Aliases**

- `GET/POST/DELETE …/aliases`

**Classify**

- `GET/POST/DELETE …/classify-rules`
- `POST …/classify-rules/reorder`
- `POST …/classify-preview` — group → credential ids (UI preview; bare group names)
- `POST …/catalog` — body: auth-file credentials + models; response: picker `entries` with `classify:` groups

Create key (plain key returned **once**):

```bash
curl -X POST "$CPA/v0/management/plugins/cpa-key-policy/keys" \
  -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "team-a",
    "name": "Team A",
    "rpm": 60,
    "account_binding": {
      "allow": ["codex-team-*.json"],
      "strategy": "weighted-round-robin"
    },
    "models": [
      {"alias":"fast","provider":"codex","target_model":"gpt-5.4-mini","group":"free"}
    ]
  }'
```

Import an existing CPA-native key for binding (the key must still exist in CPA `api-keys`; the response does not echo it):

```bash
curl -X POST "$CPA/v0/management/plugins/cpa-key-policy/keys" \
  -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "native-team-a",
    "native": true,
    "key": "existing-cpa-api-key",
    "account_binding": {
      "allow": ["openai-compatibility:corp:*"],
      "strategy": "round-robin"
    }
  }'
```

Create a multi-target alias:

```bash
curl -X POST "$CPA/v0/management/plugins/cpa-key-policy/aliases" \
  -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "alias": "cheap-chat",
    "dispatch": "priority",
    "billing_mode": "tokens",
    "targets": [
      {"provider":"cerebras","target_model":"gpt-oss-120b"},
      {"provider":"codex","target_model":"gpt-5.4-mini","group":"free"}
    ]
  }'
```

---

## Client request behavior

| Case | Result |
|------|--------|
| Known key + allowed alias | Auth OK → route → optional group filter → upstream |
| Known key + unknown model | Auth rejected |
| RPM / budget exceeded | Rejected |
| Group set, no matching auth file | `auth_not_found` / unavailable (no cross-tier leak) |
| Account binding has no eligible host candidate | `auth_not_bound` / unavailable; no global fallback |
| Bound key supplied only in query, or conflicting Header credentials | Rejected before upstream execution |
| Unknown key | Plugin declines; CPA may try native `api-keys` |
| Non-stream chat response | Top-level `model` rewritten to alias |
| Stream | Body not rewritten (v1) |

### `/v1/models` on CPA main port

Per-key `allow_models_endpoint`: **binary** — deny (401) or full global list. CPA cannot filter that list per plugin key on the main port.


---

## Setup checklist

1. Build / install the `.so` into CPA `plugins.dir`.
2. Enable `plugins` + `cpa-key-policy` in CPA config; set `state_file`.
3. Open the Web UI with the management secret.
4. (Optional) Define **classify rules** if you need custom credential buckets.
5. Create **aliases** (multi-target / pricing) and/or pick models per key (with tier or Custom group).
6. Create keys, optionally configure account-binding globs, save the one-time `plain_key`, and hand it to clients.
7. Client: OpenAI-compatible base URL = CPA; `Authorization: Bearer cpa_…`; `model` = alias name.
8. Ensure openai-compat channels list the models you map; empty model lists → host “no auth” errors.

---

## Tests

```bash
go test ./...
cd web && npm test && npm run build
```
