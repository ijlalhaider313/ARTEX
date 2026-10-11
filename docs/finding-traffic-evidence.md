# Finding Multi-Traffic Evidence

The finding detail "Associated Traffic" supports cross-page multi-select, role and notes, reordering, and unbinding. The traffic page also supports multi-selecting records to associate with an existing finding in one action. Inherited finding evidence from a task is read-only; modifications require entering the source task.

The "Agent Auto-Bind Traffic" system setting is off by default; the read and modify endpoint is `/api/settings` field `agent_traffic_binding`. Enabling it increases token consumption from request/response review, tool calls, and prompts; the next agent round uses the new setting. When disabled, auto-binding parameters and the supplementary binding tool are hidden, auto-binding guidance is not injected, and new auto-bind submissions from running sessions are rejected. Manual binding, traffic capture, saved evidence reading, and export are unaffected.

When enabled, the default flow is "finding recorded → auto-trigger report agent → verify and bind traffic → write report using the latest evidence version". The reporter retains verification commands, key output, existing real traffic IDs and their roles in `evidence`; the report agent combines finding details and execution records, verifies with `traffic_search` / `traffic_get`, then calls `bind_finding_traffic`, and reads the latest `version` to save the report. When the switch is off, the report agent does not receive raw traffic search/read tools or auto-bind; it can still read manually bound snapshots and generate reports.

Compatible with existing callers: `report_finding`'s `traffic_refs` / `evidence_hint_id` can still explicitly bind immediately. Binding is optional: for TCP and other non-HTTP findings, or when traffic was not captured or exact records cannot be found, omitting references still allows normal reporting and report writing; retaining command output, logs, and other verifiable evidence is recommended, with a note explaining why no binding was made, without adding any required fields. Submitted IDs must all be valid with complete bodies. Any single failure rolls back the entire binding operation; when explicit binding with the report fails, the entire report is rolled back. Appending the same snapshot again does not add a binding or overwrite notes.

## Agent Numbering and Report Versioning

`report_finding` adds optional parameters; array order is the initial evidence order:

```json
{
  "traffic_refs": [
    {"traffic_id": "real traffic ID", "role": "baseline", "note": "normal account request"},
    {"traffic_id": "another real traffic ID", "role": "proof", "note": "reproduction request"}
  ]
}
```

Roles are `baseline` (normal control), `proof` (vulnerability proof), `verification` (supplementary verification), `supporting` (supporting evidence, default). First verify real records with `traffic_search` / `traffic_get`; domain and time are only used for candidate filtering, not for presuming task attribution.

The prompt references [CyberStrikeAI's finding report tool guidance](https://github.com/RuoJi6/CyberStrikeAI/blob/54d56774b8bd285817d16d48b70a4a5e6e0963f7/internal/app/vulnerability_tools.go), combined with this project's optional binding convention, without introducing a required "no-binding reason" validation. Do not guess IDs, and do not re-probe solely to supplement bindings.

The first return line is still `finding recorded: <exploration node ID>`; the subsequent JSON provides the independent finding record `finding_id`, exploration node `finding_node_id`, and a binding summary.

- `get_finding_traffic(finding_id)` uses the **independent finding record ID**, returns an ordered list and `version`; pass `binding_id`, `side=request|response`, `offset`, `length` for paginated reads, each page up to 8192 bytes.
- `update_finding_report`'s `finding_id` **continues to use the exploration node ID**. The new `evidence_version` field holds the actually-read version; if evidence changes during generation, writing an old version is rejected and must be re-read and regenerated.
- Old report calls without a version do not claim to overwrite existing traffic evidence. After binding, note, role, or order changes, existing reports are flagged as pending update.

After enabling auto-binding, `add_hint` / `add_task_hint` support saving `traffic_refs` in a single hint or in each element of a batch `hints` array. The planner can pass `evidence_hint_id` when reporting on behalf, explicitly selecting references from this task's corresponding hint; inherited hints cannot be referenced. The system does not guess bindings by domain, time, or browsing history. Submission failure does not create partial findings or trigger early reports.

For existing findings with missing bindings, use `bind_finding_traffic(finding_id, traffic_refs)` to supplement; no need to re-register. `list_findings` / `list_task_findings` / `node_detail` / `get_task_node_detail` return explicit `finding_id` and `finding_node_id`; the old `id` retains exploration node semantics.

At startup, only optional properties are added to old tool schemas; the original default traffic tool binding is extended to the report agent; the supplementary binding tool is assigned to the report agent by default. Custom binding lists, prompts, descriptions, and enabled states are preserved. Guidance is added uniformly after final tool assembly: the reporting role hands off existing evidence, the report agent verifies, binds, and writes the report. Platform conversations lacking task context should hand off to the task agent via structured prompts, not report directly. Before judging a task complete, existing evidence should be handed off first; no-binding does not force waiting. A failed `report_finding` does not trigger the report agent.

## API

Base path: `/api/exploration/findings/{finding_id}/traffic`, using the independent finding ID. Uses existing authentication; `context_task` checks task visibility and inherited read-only.

| Method / Relative Path | Request / Response |
| --- | --- |
| `GET` | Ordered summary, evidence version, report-adopted version |
| `POST` | `{"traffic_refs":[...]}` batch append |
| `PATCH /{binding_id}` | `{"version":1,"role":"proof","note":"description"}` |
| `DELETE /{binding_id}` | `{"version":1}` |
| `PUT /order` | `{"version":1,"binding_ids":["2","1"]}`, must be a complete list |
| `GET /{binding_id}` | Snapshot metadata and bounded body preview |
| `GET /{binding_id}/body` | `side`, `offset`, `length`; `download=1` downloads full raw bytes |

Version/order set conflicts and writes during archiving return `409`; inherited writes return `403`; non-existent or non-finding bindings return `404`; traffic/attachment read and validation failures return explicit errors.

## Storage and Migration

Startup idempotently migrates PostgreSQL: adds `traffic_evidence_snapshots`, `finding_traffic_bindings`, and `findings.evidence_version` / `report_evidence_version` (default 0). Does not guess supplementary bindings from historical text.

Snapshots store the original traffic ID, capture time, URL, method, status, request/response headers, body length, and SHA-256. Bodies are stored by hash in `<data>/evidence/blobs/<first-two-chars>/<hash>.bin`, separate from the cleanable `data/traffic`; multiple findings can share snapshots/bodies. Snapshots do not provide a content update interface; read and export fail on checksum mismatch.

The full body is read under the original traffic write lock, including large body blobs and old directory records. Files are persisted and checksummed first, then a single PostgreSQL transaction writes the exploration node, intent relationship, finding, snapshot, and binding; the planner is notified only after commit. Failures may leave unreferenced files but do not create partial business records.

PostgreSQL advisory lock `7337741004` coordinates evidence files and SQL references; the task row lock prevents evidence modification after archive queuing. Recovery holds the evidence lock throughout from body installation to metadata commit. Deleting a finding cascades to remove bindings.

The cleaner runs hourly, reclaiming only unreferenced content not involved in ongoing operations, with at least a 24-hour delay. Normal traffic cleaning does not touch the evidence directory. Back up both PostgreSQL and `data/evidence` when backing up hot data.

## Export and Archive

Markdown includes an ordered evidence list and version; JSON includes metadata; CSV adds counts and binding IDs. `md-zip` retains the finding Markdown and provides:

```text
evidence/<finding_id>/<binding_id>/
  manifest.json
  request.http
  response.http
  request.bin
  response.bin
```

Markdown references messages via relative links. Before sending a download, attachment copying, hash verification, compression, disk sync, and CRC read verification of all ZIP entries are completed; missing/corrupted data fails the entire download. Full attachments retain binary raw bytes.

Archive v3 collects snapshots and bodies by finding binding relationships, without depending on original traffic or domains. Hot data is cleaned only after package checksum verification; shared evidence is retained. Recovery first verifies and installs bodies, then transactionally restores metadata and bindings, supporting retry on failure. v1/v2 remain recoverable; missing new fields are explicitly set to 0.

## Verification and Boundaries

Each test package uses an independent, newly created PostgreSQL test database specified via `ARTEX_PG_DSN`, to avoid residual task/model fixtures triggering background runs. Run the full relevant package tests and confirm no skips due to missing configuration:

```sh
# Set ARTEX_PG_DSN to the corresponding independent test database before running each package; explicit config failure must error.
go test ./<package> -count=1
go test -race -p 1 ./evidence ./db ./agent ./server -run 'TestEvidence|TestFindingTraffic|TestFindingEvidence|TestReportFindingAtomicContract|TestTaskArchive'
```

Frontend verification includes `npx tsc --noEmit`, Biome checks on affected files, Webpack build, and `NEXT_EXPORT=1` static export. Uses an independent cache directory to avoid overwriting a running dev server.

Local end-to-end acceptance uses independent ports, controlled HTTP / domain HTTPS targets, and a temporary data directory, covering both binding entry points, cross-page selection, ordering/notes, error messages, inherited read-only, download, and export after original traffic deletion, archiving, hot body reclaim, and recovery with hash verification.

The first version uses a global evidence coordination lock; during large batch binding/export or slow attachment downloads, other evidence operations may wait. Evidence cannot be fabricated when recording records or complete bodies are missing. This feature does not change the capture switch, nor does it handle HTTPS certificate issues when directly accessing IPs.