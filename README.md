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
6. **Model catalog policy** — reusable `/v1/models` groups can expose only selected aliases and patch advertised metadata such as context/output limits.
7. **Web UI** — manage keys, global aliases, and credential classification inside CPA.

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
| `provider` | CPA provider id (`codex`, `claude`, or an openai-compatibility **name** such as `cerebras`), or `native` to use CPA's normal mixed routing |
| `target_model` | Real upstream model id |
| `group` | Optional credential filter (see [Credential groups](#credential-groups-tiers--classify)) |
| `dispatch` | `priority` (always first usable target) or `round-robin` |
| billing | `tokens` (per-million prices) or `per_call` (fixed USD) |

Keys can **reference** aliases instead of duplicating targets. Multi-target aliases expand to several rules with the same alias name; auth and routing share one pick per request so the `group` filter matches the chosen target.

For `provider: native`, keep `alias` and `target_model` equal to a model ID
that CPA can already route. The plugin still authenticates and authorizes the
request, while CPA selects among its available native and compatibility
providers.

### Credential groups (tiers + classify)

Two sources of “which auth file may serve this request”:

| Kind | How it appears in the picker | Stored in mapping as |
|------|------------------------------|----------------------|
| **Built-in tier** (Codex `plan_type`, Antigravity `tier`) | e.g. Free tier / Team | bare name: `free`, `team`, `supported` |
| **Custom classify rule** | e.g. **Custom · vip** | prefixed: `classify:vip` |

**Runtime rule:** if a mapping sets a group, the plugin scheduler **only** picks auth files in that group. No match → hard failure (`auth_not_found`), never silently fall back to another tier.

**Scheduling:** the plugin keeps the highest available `Priority` tier, then applies **smooth weighted round-robin** using each credential's `weight`:

- Weight is read from the CPA credential candidate's `Weight`, `Attributes.weight`, or `Metadata.weight` field.
- Missing or invalid weight defaults to `1`; non-positive weight stops new requests; values are capped at `1000000`.
- State is shared globally by `provider + model + group + Priority`, so all downstream `cpa_…` keys contribute to one distribution. An empty group is one global pool containing every candidate CPA offers for that provider/model.
- Lower-priority credentials participate only when every higher-priority credential is unavailable or has non-positive weight.
- When CPA does not propagate frontend-auth group metadata (including CPA `7.2.140`), plugin-owned keys still use global weighted round-robin instead of falling back to CPA's `routing.strategy`; native CPA keys remain untouched.
- With `global_weighted_round_robin: true`, the plugin deliberately ignores group even when CPA propagates it, then schedules by Weight across every current provider/model candidate.

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
| Scheduler | Optionally filter candidates by `group`, then smooth-weight them within the highest Priority tier |
| Response interceptor | Non-stream JSON: rewrite top-level `model` back to the alias; on patched CPA hosts, filter and patch `/v1/models` catalogs |
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

`catalog_groups` are operator-owned plugin config. Each group matches one or more downstream **key IDs** (exact IDs, shell-style globs, or `*`) and defines the catalog those keys see. `include_models` and `exclude_models` accept exact model IDs or one trailing `*` prefix wildcard. The group-level `patch` deep-merges model-catalog JSON fields into every automatically included entry; `remove` deletes top-level fields. Exact `models` entries can still clone/rename a `source` and add a model-specific patch.

```yaml
catalog_groups:
  - name: pi-coding
    keys: ["team-*", "dev"]

    # Keep model families without enumerating every generated catalog entry.
    # Matching is case-insensitive; only exact or one trailing "*" is valid.
    include_models:
      - "gpt-5-*"
      - "claude-*"
    exclude_models:
      - "gpt-5-codex-*"

    # Deep-merge these fields into every retained source catalog model.
    # This is catalog metadata patching, not an HTTP PATCH operation.
    patch:
      context_window: 262144
      max_context_window: 262144
      max_tokens: 32768
    remove:
      - upgrade

    models:
      # Explicit entries still clone/rename a source. Their patch is merged
      # after the group patch, so the model-specific value wins.
      - id: fast
        source: gemini-3.8-flash-high
        patch:
          max_tokens: 16384
          display_name: "Fast"

      # source defaults to id when omitted.
      - id: gpt-5.4
```

Notes:

- If `state_file` exists, it is the source of truth for keys / aliases / classify rules / usage. `catalog_groups` remain in plugin config so state persistence cannot erase operator-owned catalog policy.
- `include_models` supports exact names and a single trailing `*` prefix wildcard. `exclude_models` uses the same grammar and always wins across all groups matched by the key.
- A group-level `patch` / `remove` applies to entries selected by `include_models` or `include_unlisted`, and is inherited by explicit `models`. A model-specific patch is merged afterward and wins on conflicts.
- `include_unlisted: true` is equivalent to including every source model; group patch/remove and exclusions still apply. The default remains `false` for strict per-key allow-lists.
- `allow_models_endpoint` is still the security gate. A catalog group never grants access to `/v1/models`; the key must already have `allow_models_endpoint: true`.
- When at least one `catalog_groups` entry exists, a plugin key matching no catalog group receives an empty model list. Native CPA keys and other auth providers are left untouched.
- A catalog item whose `source` is absent from CPA's generated catalog is omitted rather than synthesized with guessed capabilities.
- Catalog filtering requires a CPA host that exposes `GET /v1/models` through the existing `response.intercept_after` plugin hook. Without that host patch, the old binary behavior remains.
- `global_weighted_round_robin: true` ignores the selected alias target group and places every current provider/model candidate in one global pool. Distribution then follows the Weight values on CPA's credential page. The default is `false`.
- With this option enabled, alias-level group rotation no longer restricts the final credential. Native CPA keys remain unaffected.
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
    "models": [
      {"alias":"fast","provider":"codex","target_model":"gpt-5.4-mini","group":"free"}
    ]
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
| Unknown key | Plugin declines; CPA may try native `api-keys` |
| Non-stream chat response | Top-level `model` rewritten to alias |
| Stream | Body not rewritten (v1) |

### `/v1/models` on CPA main port

`allow_models_endpoint` remains a per-key deny/allow gate. On a CPA build with the model-list response-interceptor patch, allowed plugin keys are additionally filtered through `catalog_groups`:

- matching groups are combined in config order;
- `include_models` selects exact IDs or model families using one trailing `*`; `exclude_models` always wins globally;
- bulk-selected models preserve CPA catalog order and identity while receiving the group's deep field `patch` / `remove`;
- duplicate exposed `id` values keep the first entry;
- explicit `source` metadata is cloned from CPA's real generated catalog and may be exposed under another `id`;
- model-specific `patch` values override inherited group patch values; nested objects merge recursively;
- `remove` deletes selected top-level fields;
- `include_unlisted: true` includes every source model while still applying group patches and exclusions;
- unmatched plugin keys get an empty catalog when catalog groups are configured;
- native CPA keys are not filtered by this plugin.

For Pi/Codex clients (`/v1/models?client_version=...`) the plugin preserves the rich catalog shape and changes the cloned model's `slug` to the downstream `id`. For ordinary OpenAI clients it changes `id`.

---

## Setup checklist

1. Build / install the `.so` into CPA `plugins.dir`.
2. Enable `plugins` + `cpa-key-policy` in CPA config; set `state_file`.
3. Open the Web UI with the management secret.
4. (Optional) Define **classify rules** if you need custom credential buckets.
5. Create **aliases** (multi-target / pricing) and/or pick models per key (with tier or Custom group).
6. Create keys, save the one-time `plain_key`, hand out to clients.
7. Client: OpenAI-compatible base URL = CPA; `Authorization: Bearer cpa_…`; `model` = alias name.
8. Ensure openai-compat channels list the models you map; empty model lists → host “no auth” errors.

---

## Tests

```bash
go test ./...
cd web && npm test && npm run build
```
