# api-recon — Reference Manual

Grep recipes, `config.json` template, and troubleshooting. All greps target the `js/` directory. When bundles are single-line, run `js-beautify` or `sed 's/}/}\n/g'` first; usually a raw grep with a context window suffices.

## Script Notes

All files in `scripts/` are **reference templates** and must be adapted to the target site before execution. Typical adjustment points:

| Script | Common adjustments |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success value, stub match rules and body structure, routes source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookies, depth/max, same-domain filter |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | unified entry point for all the above site-specific parameters |

Adjusted files should be placed in the task working directory (e.g. `recon/`); note specific changes relative to the reference scripts in the report.

---

## A. Reverse-Engineering Three Gates

### A1. Render Gate — "How does it determine logged-in?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))`, identifying the **storage key**, **container** (Cookie vs localStorage), and **encoding**:

| Encoding | config forge method |
|---|---|
| Plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | Unsigned/`alg:none` JWT, or sign with key found in bundle |
| Encrypted (SM2/AES/RSA) | Find hardcoded key; render gate only needs a decodable blob to forge; otherwise static fallback |

→ Write into `cookies` / `localStorage`.

### A2. Interceptor Gate — "What triggers redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(not logged in|please log in again|login expired|unauthorized|session expired|authorized|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Identify: **field name**, **success value** (usually `0` or `200`), **failure value that triggers redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Write into `neutralize.fields` + `neutralize.success`.

### A3. Content Gate — "Where do menus/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise backends):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | Route guard, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | Sidebar menu rendering |
| In-bundle `userRouteAuth` | `{ CODE: { url, name? } }` | code → frontend path |
| In-bundle `routeMap` | `{ KEY: { name, link } }` | Alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): construct a permissive payload → `stubs`.

**Full permission tree restoration** (large sites, sidebar/submodules still blank): see **Section I**.

---

## B. config.json Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field notes:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), `both`
- `cookies[].value` prefix: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal
- `forward: true` forwards real requests and rewrites code fields; `false` fully offline stub
- `mockTier`: coverage-mode preload enablement level, e.g. `L1+L2`, `L1+L2+L3`
- `routes` come from `routes.txt`; after forging menus, the harness auto-appends `<a href>`
- `captureResponses` / `recordWs` only effective in depth mode
- `waitUntil`: use `domcontentloaded` for large SPAs, avoid `networkidle2` hanging
- `routeTimeout`: per-route `page.goto` timeout (ms)
- `proxy`: Puppeteer `--proxy-server`; can also set `HTTP_PROXY` / `HTTPS_PROXY`

### B1. Dual-stub Template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

Outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must cover all leaf codes in the tree.

---

## C. Coverage Mode: preload Configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace before CDP injection:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify: `window.__API_RECON_PRELOAD__ === true` and pathname is stable.

Export recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook Capabilities

Browser hook capabilities built into preload (coverage) and runtime_harvest (depth), and their coverage:

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | Record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Discover Authorization etc. headers | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie read | Confirm session key names | ⚠️ optional `observe.storageReads/cookieReads` |
| Vue route access | Complete frontendRoutes | ✅ `__API_RECON_ROUTES__` (loaded routes) |
| Vue route guard neutralization / login redirect blocking | Expand modules to trigger APIs | ✅ `neutralizeVueRouter` + native redirect neutralization |
| React route access | Supplement routes | ⚠️ static + click; no dedicated hook |
| Page redirect blocking (login path) | Stay on page for analysis | ⚠️ only blocks login path, avoids blocking business navigation |
| Hook crypto library (CryptoJS/SM etc.) | Encrypted params → plaintext API body | ❌ must manually hook crypto function inputs; write conclusions to config |
| Anti-debug bypass | Otherwise runtime cannot record APIs | ❌ must handle manually; static still works |

---

## E. Endpoint Extraction Regex (when static yield is low)

Loosen `extract_endpoints` in `harvest_static.py`, or manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → Fix |
|---|---|
| Very few static APIs | Endpoint dialect mismatch → loosen regex (Section E) |
| Chunk count ≪ manifest | CSS-only or undeployed chunks; 404s already retried |
| Runtime still shows login page | Render gate wrong → recheck A1: key name, container, encoding, domain |
| Gets past shell but modules blank | Content gate → forge menus (A3); `routes` paths may be wrong |
| Only bootstrap/locale per route | Permission codes incomplete → Section I permission tree restoration; check `role_permissions` + `permissions/all` dual stub |
| Sidebar has items but subpages blank | Tree missing intermediate nodes or code mismatch with `userRouteAuth` |
| Every API redirects to login | Interceptor gate → confirm `neutralize`; nested fields need extended walk logic |
| WS frames = 0 | Need user interaction before subscribe; increase `perRouteMs` |
| Response body empty | Only `forward: true` has real responses |
| Chromium missing | Install chromium or set `config.chromium` / `CHROMIUM` |
| Lots of mocks but still login | Hook too late or missing `location.href` setter → document-start + preload |
| Lists all empty | L3 empty arrays are normal; keep clicking Tab/settings/details |
| Mistaking Redux actions for routes | Filter internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload not at document-start → fix injection timing; or manually clear guards when `neutralizeVueRouter: false` |
| Response has URLs but not in log | Enable `extractUrlsFromResponse`; or manually extract from `__API_RECON_DETAIL__` |
| Don't know Authorization header name | Enable `observe.xhrHeaders` or check request headers in DevTools |
| Runtime very slow / timeout | Use `waitUntil: domcontentloaded`; lower `routeTimeout`; don't use `networkidle2` |
| Proxy connection failed | Check `proxy` / env vars; Puppeteer and curl proxy ports must match |

---

## G. Hardened Targets

When the server progressively validates sessions (un-forgeable signed cookies, server-rendered menus that can't be stubbed), runtime will stall at the shell. Expected behavior:

- **Static is sufficient for endpoint enumeration** — module paths are in the code
- If authorized, run the same harness with a **real session**: `forward: true`, no neutralize needed, capture real methods/params/responses

---

## H. Single-Task Checklist

1. Confirm authorization scope
2. **Read** `scripts/harvest_static.py` → adapt to target → run → review `api_static.txt`, `routes.txt`
3. **Phase 1b**: path anchor window expansion + binding layer → `param_candidates.json` (Section J)
4. Reverse-engineer A1/A2/A3 → write site-specific `config.json`
5. **Read and adapt** `runtime_harvest.js` / `preload.js` before executing
6. `runtimeMode=depth`: `npm install` → run adapted harvest script
7. `runtimeMode=coverage/both`: document-start inject adapted preload → browser MCP dynamic enumeration + **parameter trigger matrix**
8. Modules not rendering → **Section I permission tree restoration** → patch stubs → rerun
9. Multi-sample parameter diff + error inference → `params_merged.json`
10. Merge → `site_map.json` + `api_merged.txt`, honestly annotate coverage, gaps, and script changes

---

## I. Permission Tree Restoration (Phase 4 Deep Dive)

Use when forging simple `menus: [{ path, show: true }]` doesn't work and submodules still don't mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: **permission API path**, **response field names**, **consuming chunk filename**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# Output: recon/route_map.json
```

If `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or manually grep:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases `He=o.DASHBOARD`)
2. Use `route_map.json` to resolve aliases → real paths
3. Infer parent by code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, auto-writes `config.json`'s `stubs` and extends `routes`

**Adjust for target** (at the top of the script):
- `DEFAULT_ROOTS`: top-level module code list
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes with non-prefix relationships

### I4. Validate stub consistency

```bash
# permissions count should ≈ userRouteAuth entry count
wc -l recon/perm_codes_all.txt
# routes should cover all route_map links
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# Compare runtime_api.json entry counts before and after forging; check whether /attack, /asset etc. module APIs appear
```

| Before forge | After forge (success) |
|---|---|
| Same 3–5 bootstrap entries per route | Different routes trigger different module APIs |
| Only `/api/locale/language` | `/api/web/...` module endpoints appear |
| `routes.txt` has single-digit routes | `routes` 80–110+ from route_map |

### I6. Still failing

- **Coverage mode**: click sidebar + Tab; permission gating may only request on interaction
- **Stub fields**: compare real API (curl + real session) vs stub nesting
- **Additional guards**: grep `hasPermission|checkRole|func.` etc. button-level checks, extend `role_permissions.permissions`
- **Static fallback**: module API paths are still in `api_static.txt`, runtime only supplements METHOD/body; parameters retained in `param_candidates.json` + recorded samples

---

## J. Parameter Reverse-Engineering (Phase 1b / 5b / 5c)

**Methodology, not a universal script.** Find paths with regex; find parameters with anchor window expansion + UI binding chain + multi-sample diff + error inference.

### J1. Anchor window expansion — from path to request-building object

```bash
# Use Phase 1 known path as anchor
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper layer and transport shape

```bash
# axios / unified request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# Path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation gate — required / format / enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding layer — form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Runtime supplement: DevTools → Network → request → **Initiator** (call stack) trace up from `fetch`/`send` to the request-building function.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Don't guess fields on ciphertext** — hook the crypto function **inputs**, record the plaintext payload before encryption; write conclusions to `config.json` / `param_candidates.json`.

### J6. Parameter trigger matrix (Phase 3 required)

Record once per module per operation, diff request body/query:

| Operation | Focus |
|---|---|
| List first screen | Pagination defaults |
| Search | keyword, filters |
| Advanced filter | Optional fields |
| Create/Edit | Full entity |
| Batch/Export | `ids[]`, `exportType` |
| Sort/Pagination | `sortField`, `order` |

Output `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **High** | Static callsite + runtime ≥2 consistent samples |
| **Medium** | Static only, or only 1 runtime |
| **Low** | Inferred from response/error, not second-verified |
| **Pending trigger** | Static-known field, UI/permission not yet reached |

### J8. Scenario quick-config

| Scenario | Sequence |
|---|---|
| REST list page | J1 request object → J6 four-way diff → J3 rules |
| Create/Edit form | J3 Form name → J4 submit chain → runtime submit + intentionally leave blank to see 400 |
| GraphQL | J2 variables declaration → runtime record variables per operation |
| Encrypted body | J5 hook inputs → pre-encryption fields are the real params |

### J9. Mapping to api-recon phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 static | J1 anchor window expansion |
| Phase 2 A2 interceptor | Global injection fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | Different modules have different forms → full fields only triggered with sufficient permissions |
| Phase 5 merge | `params_merged.json` + confidence; don't determine required from a single sample |

### J10. Troubleshooting

| Symptom | Fix |
|---|---|
| Static has field names runtime never shows | Mark "pending trigger"; supplement permission tree / click advanced filter / linked select each option |
| Same path different body shape | Normal — record separately by `action`, don't force-merge schema |
| Stub response is fake but want to see params | **Look at outbound request** body/headers, don't infer from stub response |
| 400 reports nested field | Note outer wrapper `data`/`bizData`/`variables` |
| GraphQL only shows operation name | Expand `variables` JSON; statically find `$var: Type` |

---