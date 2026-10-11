---
name: api-recon
description: Invoke this skill when collecting website API endpoints.
---

# API Recon (Frontend Endpoint Reconnaissance)

Under **authorized** conditions, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature trigger points** (tabs, modals, table actions, etc.).

---

## Boundaries & Prohibitions (Agent Must-Read · Violating = Out of Scope)

This skill **only performs API / parameter-surface reconnaissance**, not vulnerability hunting or exploitation.

### Task Boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate path, method, parameters, routes, UI trigger points | SQLi/XSS/IDOR/brute-force/fuzz vulnerabilities, modified-packet attacks, destructive operations |
| **Auth** | Hook + stub/mock to bypass **client-side** login gates | Asking user for or guessing account passwords; attempting real login form submission |
| **Runtime** | Hook endpoints without credentials, use mock responses to let SPA enter the post-login shell | Flows that depend on a real backend session to continue |

### Credential-Free Dynamic Analysis (Phase 3 Default)

1. **Intercept and stub** login, permission, menu, and other bootstrap endpoints via `preload.js` / `runtime_harvest.js`;
2. Return **structurally correct, success business-code, data-may-be-empty** mock bodies for business query endpoints;
3. Enable the frontend to render post-login pages even without a backend or under 401 conditions, thereby triggering more XHR/fetch/WebSocket calls;
4. **Empty data, blank tables, placeholder UI blocks are expected** — do not pivot to real login or vulnerability testing for this.

**In one sentence**: Use mocks to expand frontend routes and component mounting, **only record outbound requests**; what the backend returns doesn't matter — what matters is **what other endpoints the frontend still sends**.

### Hard Process Prohibitions

| Prohibited | Alternative |
|---|---|
| grep/curl/Read main entry `index-*.js` to extract API paths before Phase 1 completes | Run `OUTDIR/harvest_static.py` |
| Hand-writing `extract_apis.py` or similar scripts to replace harvest | Modify `OUTDIR/harvest_static.py` and re-run |
| Repeating the same grep/command after ≥2 failures | Switch strategy: read tool_logs, modify harvest, consult reference |
| Skipping gates A/B, directly running originals from `scripts/` | Copy to OUTDIR and adapt for the target |
| Real username/password, OTP, OAuth, etc. authentication | stub/mock (see above) |
| Skipping stub under the pretext of "getting real data", doing IDOR/injection testing | Only record outbound; that's recon scope |
| Delete, export sensitive data, batch write, and other irreversible operations | Coverage clicks are the same |
| Claiming all pages and endpoints obtained without completing runtime + dynamic enumeration | See "Completion Definition" or note limitations |
| Claiming all parameters mastered without completing parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff or reverse-engineer from validation rules/errors |

---

## Two-Layer Model + Run Modes

| Layer | Output | Limitations |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, draft routes, call-site field candidates | No HTTP methods; parameters need Phase 1b; misses runtime-assembled URLs |
| **Runtime** (live session) | Method + body + response + dynamic URL + WS/SSE; multi-sample diff completes parameters | Pages must actually render to send requests; single sample insufficient for required/optional |

| Run Mode | Engine | Use Case |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API list, METHOD/params/response body, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Click tabs/modals/tables, deeper feature-point coverage |
| **both** | depth then coverage | Most complete, most time-consuming |

**Parameter methodology** (no universal script): paths via harvest/regex; parameters via **anchor window-expansion + UI binding chain + multi-sample diff + error reverse-inference** (grep recipes see [reference.md](reference.md) section J).

---

## Completion Definition

All must be satisfied before claiming recon complete:

- [ ] **Static**: Phase 1 harvest produces `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: At least depth or coverage; coverage/both requires **Hook active + dynamic enumeration loop**
- [ ] **Shell entry**: Accessing business paths is not `/login` (note hash routing)
- [ ] **Parameters**: coverage/both completes parameter trigger matrix + `param_samples.json`; Phase 5 merges `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission tree restored and re-run until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs complete (see Phase 5 output table); `insert_assets` writes service and endpoint assets

---

## Scripts & Gates

`scripts/` are reference templates only — **prohibited** to run originals directly and treat as final results.

**Rules**: Read first → adapt for target → write to `OUTDIR` (e.g. `recon/`) → record `CHANGES.md`; if not matching, rewrite per methodology, only borrow structure.

| Gate | When | Reference Script → OUTDIR Copy | Common Must-Change Items |
|---|---|---|---|
| **A (Static)** | After Phase 0, **first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **Most sites' default regex runs directly**; only change endpoint regex, webpack/Vite `publicPath`, MPA exclude/cookie when manifest/dialect doesn't match |
| **B (Runtime)** | After Phase 2, before depth/coverage run | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**SPA mandatory order** (non-exchangeable; Phase numbering takes precedence over "explore first, then script"):

| Step | Required | Prohibited |
|---|---|---|
| After Phase 0 | Next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read main entry `index-*.js` (usually >500KB) |
| Gate A | Copy script → minor changes as needed → **run immediately** | Manually extract APIs first then decide whether to harvest |
| Before Phase 1 completes | `wc -l` verify outputs; 404 → fix harvest and retry | Hand-write extract scripts; repeatedly grep un-downloaded URLs |
| From Phase 1b | grep only `OUTDIR/js/*.js` | Use main bundle instead of harvest |

- ✅ Copy `harvest_static.py` → (optional) change regex → **run immediately**
- ❌ curl main bundle → grep multiple times → write temp extract → harvest only at the end
- **MPA**: After Phase 0, next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool & Output Constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` >100KB **prohibited** from Read/grep into context; use OUTDIR scripts for batch processing |
| grep output | Must `\| head -20` or `-m 5`; keep only path summaries in conversation, don't paste bundle fragments |
| Validation | Use `wc -l`, `ls \| wc -l`; don't Read entire directories |
| Regex probing | Optional, ≤1 time, only on ≤50KB small chunks or HTML; authoritative static results come from harvest |
| reference | Recipes/templates/troubleshooting see [reference.md](reference.md), don't repeat full content inline |

---

## Execution Roadmap

```
Phase 0 Classify + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reverse-engineering
  → Phase 2 auth three gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (if needed) → re-run Phase 3
  → Phase 5 merge report + insert_assets batch insert all discovered services, endpoint API assets; never omit discovered assets when inserting
```

Check off in order; **do not proceed to the next Phase until the previous one completes**.

1. [ ] **Phase 0**: Initial probe SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: Copy script → **immediately** harvest → `wc -l` verify → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: Anchor window-expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: Auth three gates → `config.json` → [Phase 2](#phase-2--auth-three-gates)
5. [ ] **Gate B**: Adjust runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm shell entry; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): Permission tree → patch stubs → re-run Phase 3 → [Phase 4](#phase-4--permission-tree-restoration)
8. [ ] **Phase 5**: Merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch entry HTML, **create `OUTDIR`** (don't modify skill's `scripts/`):

- **SPA**: Empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produces `forms.txt`, `links.txt`, `api_inline.txt`. SPA if forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts & Gates](#scripts--gates) · [Tool & Output Constraints](#tool--output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML script → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- Chunk count vs manifest: 404s require fixing harvest and retrying, don't manually curl each chunk
- `api_static.txt` too few → relax endpoint regex in OUTDIR and re-run (see reference)

### Phase 1b — Parameter Reverse-Engineering

Paths come from Phase 1; parameter fields need separate recon. grep rules see [Tool & Output Constraints](#tool--output-constraints).

**Completion criteria**: For each important endpoint, answer — field names, transport location, type inference, required/optional, sample values, confidence.

#### 1b.0 — Transport Forms

| Form | Where parameters are | Static priority |
|---|---|---|
| REST JSON | body + query | next to path anchor `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameters | `/user/:id` | route table + `useParams` / `$route.params` |
| Encrypted/signed | wrapped in `sign`/`data` | Hook encryption function inputs (reference section D) |

Output: mark each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor Window-Expansion

Use known paths as anchors, expand window to find payload-assembly objects:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper Layer | Parameter Clues |
|---|---|
| axios instance | `data` / `params` |
| Unified request | interceptor injects global fields |
| OpenAPI client | generated method signatures |
| React Query / SWR | hook second argument |
| Vue composable | composable input parameters |

Type residuals: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding Layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding Source | Technique |
|---|---|
| Form submit | follow submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

DevTools call stack: trace from `fetch`/`XHR.send` upward to the payload-assembly function.

#### 1b.3 — Payload Assembly Three Questions (≠ Phase 2 auth three gates)

| Question | What to Answer |
|---|---|
| **Assembly** | where payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

Interceptor gate (Phase 2) also reads globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Handoff to Phase 3

Candidate fields come from static/binding layer; **required/optional/conditional dependencies** need Phase 3 parameter matrix + diff + Phase 5 error reverse-inference.

---

## Phase 2 — Auth Three Gates

grep in `OUTDIR/js/` (with `head`), write to `config.json` (recipes see reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How is logged-in determined? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menu/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat localStorage key names as credentials — must confirm from chunk/request chain.

**Exit = Gate B**: conclusions land in `config.json`, and modify `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API Observation (Optional)

Use OUTDIR's `preload.js` to confirm session key names, Authorization, nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observation |
| `extractUrlsFromResponse: true` | sub-APIs in responses |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Coverage exports each round: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Must have passed Gate B; follow [Boundaries & Prohibitions](#boundaries--prohibitions-agent-must-read--violating--out-of-scope) · credential-free mock strategy.

`config.json` sets `"runtimeMode": "depth" | "coverage" | "both"` (template see reference).

### Hook & Stub (depth + coverage shared)

| Layer | Scope | Purpose |
|---|---|---|
| L1 Precise | auth/permission/bootstrap stub | Pass first-screen auth |
| L2 Negative correction | All JSON responses | Not-logged-in code → success |
| L3 Fallback | `/api` etc. not hit by L1 | Empty success body, expand UI |

- **depth**: fake auth + `forward` to change business codes + `stubs`; traverse `routes` (hash/history); produce `runtime_api.json`
- **coverage**: **document-start** inject `preload.js` (CDP `addScriptToEvaluateOnNewDocument` or Userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business path doesn't return `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — Coverage Dynamic Enumeration (Mandatory)

1. Main nav/sidebar — click each item, wait for network 1–3s
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — first row view/edit/detail
4. Toolbar — export, filter, create (**avoid irreversible delete**)
5. Each module entered — merge APIs/routes
6. SPA — controlled `pushState` for paths not covered by `routes.txt` (MPA prohibited)

**Parameter trigger matrix** (mandatory): record once per operation type per module, **diff multi-sample**:

| Operation | Usually extra parameters |
|---|---|
| List first screen | pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | more optional |
| Create/edit | full entity |
| Batch/export/sort | `ids[]`, `exportType`, `sortField` |

**Under stub, outbound body/headers are still real** — trust the request. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — Permission Tree Restoration

**Trigger**: Module pages blank / each route only bootstrap (e.g. locale) → content gate not passed.

| Symptom | Meaning |
|---|---|
| Shell entry succeeds | Render gate + interceptor gate passed |
| Sidebar missing items / blank on click | stub shape or permission codes incomplete |
| Each route's API is the same and minimal | `v-if permission` not passed |
| `routes.txt` far fewer than bundle | need to complete from auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

stub check: outer `response_code` matches interceptor gate; flat codes align with tree; `routes` covers all links in `route_map`.

Update `config.json` then **re-run Phase 3**. Large SPAs can tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference A3/I section).

---

## Phase 5 — Merge & Report

### Output Table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundle and paths |
| `param_candidates.json` | 1b | Static parameter field candidates |
| `config.json` | 2 | Three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | multi-sample, click log, detail |
| `route_map.json` etc. | 4 | Permission tree intermediate files (if executed) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | Write all services, endpoint assets to asset library |

### 5b — Parameter Merge

Diff from `param_samples.json`, **no universal merge script**. Confidence rules see reference J7 (high/medium/low/pending-trigger).

### 5c — Error Reverse-Inference

Within authorized scope, can send incomplete requests to read 400s (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Note `data` wrapping, `variables`, pre-encryption `bizData`.

Report must note: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, `CHANGES.md` summary relative to reference scripts.

`site_map.json` suggested structure:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

More fields and grep recipes see [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: webpack/Vite/Angular lazy-load methods are the same
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web out of scope
- **SSR**: Client-side fetch can be recorded; RSC/Server Actions not fully enumerable
- **Blind spots**: JSVMP, WASM, HMAC/mTLS strong validation → static + note limitations
- **Parameter blind spots**: conditional linkage, hidden params, WASM payload assembly → "pending-trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` templates, troubleshooting, Hooks, parameter reverse-engineering section J, site_map templates: **[reference.md](reference.md)**
- Reference script paths see [Scripts & Gates](#scripts--gates) table