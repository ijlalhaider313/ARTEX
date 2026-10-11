# Changelog

All notable changes to this project are documented in this file, following the [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) format.

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### Added

- **Added a built-in interception rule "Delete-type endpoint paths"**: previously the built-in HTTP destructive rules only recognized the DELETE **method**, while most applications' delete endpoints can be triggered via GET/POST, so calls like `curl 'http://t/api/user/delete?id=1'` would actually delete target data. Now a `deny` rule has been added covering `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, and `/delta` are not falsely intercepted). Existing instances also get it after upgrading, and it can be disabled or removed under "System → Command Interception".
- **Token consumption of model fallback approval can now be viewed separately**: approval calls previously had no independent metering attribution, so how much was spent could only be guessed. Now it is metered separately, and a usage card has been added below the "Model fallback approval" toggle in "System → Command Interception" — cumulative statistics for five items: call count, input, output, and cache reads/writes, plus a mini bar chart of daily consumption for the last 30 days.

### Vulnerability Push

#### Added

- **Vulnerability findings now support pushing to IM**: a new "System → Notification Push" page has been added, allowing scan findings to be pushed to six channels: **DingTalk, Feishu, WeCom, generic Webhook, Telegram, and email**. Any number of instances of the same type can be configured, each with its own enable/disable, rate limiting, and filter rules.
- **Push timing has two modes: real-time and digest**: in real-time mode each match is sent one by one; in digest mode, items are merged into a single message per the global period (default 30 minutes), with the new count and severity distribution at the beginning. To do "critical real-time, rest in digest", create two channels and configure them separately.
- **Filter rules support four dimensions**: minimum severity, task/asset scope restriction, keyword inclusion and exclusion for vulnerability types (exclusion takes priority), and whether to receive disposition status changes (default off). Messages carry a "View details" button; the target address is configured by the global "Callback link address", and leaving it empty means no button.
- **Delivery history and manual resend**: lists each delivery's status, attempt count, failure reason, and channel, filterable by channel and status; failed items can be resent with one click (resending resets the retry counter).
- **Channel credential mask echo**: the Webhook address, signing secret, Bot Token, SMTP password, etc. are only echoed as masked values with a suffix hint; submitting them unchanged means "no modification", and clearing the input deletes that field.

#### Fixed

> This section is the result of a security audit conducted after the initial version of the feature; each item was first reproduced, then fixed, then covered with a regression test.

- **Fixed masks being bypassable via "change target address, keep credentials" (severe)**: the config merge preserved the original stored value for any "unmentioned key", so changing only the address while saying nothing about credentials would let the server send the real stored credentials to an arbitrary address, completely silently. Now, once the target address changes, every credential field must be explicitly declared — either given a new value or explicitly left empty to indicate it is no longer needed.
- **Fixed zero escaping of untrusted content in markdown-based channels**: vulnerability titles and summaries come from model output, and asset URLs come from scan results, so a crafted title would render as a clickable external link in DingTalk/WeCom/Feishu, and image syntax would be fetched by the client at render time and leak the reader's IP. Now everything is normalized to a single line + markdown metacharacter escaping, and escaping happens at each channel's rendering output rather than in the shared title function.
- **Fixed the SSRF surface of delivery addresses**: previously loopback, internal network, and cloud metadata addresses could all be delivered to, and the first 200 bytes of the failure response body would end up in `last_error` and be echoed in the delivery history, forming a semi-blind read primitive. Defense is now placed at the **dial phase**, covering DNS rebinding and rejecting cross-host redirects; loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private networks remain allowed (self-hosted internal SMTP relays are common).
- **Fixed push credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key`, and Telegram's bot token are all in the URL, and when delivery fails the full URL would flow into four places: `last_error`, the delivery history, server logs, and frontend prompts. Now it is uniformly redacted, keeping only `scheme://host` and the underlying reason.
- **Fixed silent loss caused by marking an entire truncated digest batch as delivered**: truncated vulnerabilities were neither in the message nor in the failure list, yet the delivery history showed success. Now items are packed as **whole entries**, and only those that fit in this message are marked; the rest return to the queue to be sent in the next message without consuming retry counts, and the message header states how many more will continue in the next message.
- **Fixed a typo in the minimum severity threshold silently disabling the filter**: when `min_severity` is written as a typo like `hgih`, the unknown severity ordinal is 0 and the check degrades to always true — the user thinks only critical items are received, but actually all vulnerabilities get dumped into the group. Now the value is validated on the write path and valid options are listed.
- **Fixed `rate_per_min = 0` (no rate limit) being unreachable**: the DB write layer silently replaced an explicit 0 with the default value, so the operator thought rate limiting was lifted while actually still being throttled. Default value filling has been moved to the API layer, applied only when the field is absent.
- **Fixed digest channels completely bypassing the token bucket**: `rate_per_min` previously had no effect on digest mode. Now the number of items claimed is constrained by both the current round's quota and an in-memory upper bound.
- **Fixed failure disposition being judged by the batch's maximum attempt count, dragging new deliveries down with old ones**: an old delivery that had already been retried twice would drag brand-new deliveries in the same batch into failed, so new vulnerabilities were permanently lost without even using one retry. Now it is decided per item.
- **Fixed deliveries with unparseable snapshots being silently marked successful in digest batches**: such entries would be skipped at the render stage, yet counted as successful along with the whole batch. Now they are explicitly judged as failed with a reason.
- **Fixed per-channel per-round delivery counts potentially exceeding the lease duration**: in multi-instance deployments, rows with expired leases would be re-claimed, resent, and have their attempt counts incremented twice. Now the per-round cap is derived backwards from "lease / single timeout", with an assertion pinning the relationship between these constants.
- **Fixed Telegram truncation potentially cutting HTML entities**: truncation only avoided half-cut tags but not entity fragments like `&amp`, which could cause the parser to reject the **entire** message. Now both are avoided.
- **Fixed the email channel judging temporary SMTP failures as permanent**: 4xx codes like greylisting `450` are temporary rejections that should be retried later, but were previously all judged fatal, so servers with greylisting enabled would fail every push after the first attempt. Now they are distinguished by the first digit of the response code: 4xx is retryable, 5xx is a permanent failure.
- **Fixed nested-structure submissions actually writing the mask literal into the database**: object fields like `webhook.headers` can only be masked or submitted as a whole; stuffing the mask sentinel inside an object would be stored as a real value and cause silent authentication failure. Such submissions are now explicitly rejected.

#### Design Notes

- **The vulnerability-writing transaction performs only one blind INSERT**: `notification_events` and the vulnerability are written in the same transaction, so committing guarantees atomic consistency between the two. This INSERT deliberately does not read the channel table or run the user's filter rules, and is wrapped in a `SAVEPOINT` — otherwise a single misconfigured filter condition could abort the transaction and prevent a critical vulnerability from being stored.
- **Delivery uses lease-based claiming rather than long transactions**: after claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as a lease, so no database lock is held during delivery; rows left behind by a crashed process are re-claimed after the lease expires, self-healing without infinite retries.
- **Rate limiting does not consume the retry budget**: first compute the current round's quota from the token bucket, then claim that many. Reversing the order would waste an attempt count on deliveries blocked by rate limiting, exhausting the three-attempt budget through pure waiting and falling into failure.

### Test Infrastructure

#### Fixed

- **Fixed permanent asset residue caused by cleanup ordering in the company ICP attribution test**: `defer d.Close()` ran first when the function returned and `t.Cleanup` after it, so all cleanup statements landed on an already-closed connection, errors were discarded, and test assets and companies remained permanently in the database, also causing other asset-count-assertion tests to fail mysteriously. Now closing the connection also goes through `t.Cleanup` and is registered first, while cleanup failures are surfaced.

### Traffic

#### Added

- **Traffic list now supports content search, path/status-code/length filtering, and column sorting**: the filter row supports filtering by **response content** (via full-text index), **path**, **status code** (exact value or buckets like `2xx`), and **response length range**; column headers can sort by time, status code, and response length, with sorting preferences stored locally. Two indexes were added and take effect automatically on existing instances after upgrade; the Agent tool query contract is unchanged.
- **Traffic list gains "Clear all"**: deletes all traffic records at once (ignoring the current filter), and cleans up historical host directories no longer recorded by the index; after clearing, a full compaction is performed and the actual freed space is reported. Traffic evidence bound to vulnerabilities lives in a separate evidence store and is unaffected.

#### Fixed

- **Fixed disk space not being freed after deleting traffic**: the index database was created without `auto_vacuum`, so the file never shrank; and the full-text index used `contentless_delete`, where `DELETE` only writes a tombstone and does not reclaim the original postings — deleting traffic actually made the index bigger (measured: capturing 6MB of body text → 16MB index, still 16MB after deleting everything). Now new index databases enable `auto_vacuum=incremental`, and after each delete, full-text index merging and incremental reclamation run in chunks in the background, gradually returning space to the system (down to 104KB in the same scenario); reclamation releases the write lock between chunks and does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set at database creation time, so **existing instances' index databases remain in the old mode**, and incremental reclamation is a no-op on them (a one-line notice is printed at startup). For such databases, tombstone growth has stopped after the upgrade, and the already-occupied space can be reclaimed once with "Clear all" — that step converts the database to incremental reclamation mode, after which routine deletes take effect on their own.

### LLM

#### Fixed

- **Fixed custom session headers not taking effect on "one-shot LLM call" paths**: goal decomposition (round 0) and cold-node compaction are both one-shot calls without a transcript store, so there is no session id on the context and the gateway cannot read the header — for endpoints like opencode zen that return 400 outright when the header is missing, this manifests as "round 0 fails, but subsequent planner rounds work fine". Now these two paths explicitly attach a session id that is stable per exploration, which also lets token usage be correctly attributed to the corresponding exploration (previously not recorded at all).

### Tools

#### Fixed

- **Fixed output truncation not working for MCP and custom tools, with large results flooding the context in full**: MCP tool returns previously went through no truncation at all, while HTTP-type custom tools, due to an empty tool context being passed in, always used a hardcoded 6000 characters instead of the session-configured limit. Now both paths go through `actool.Capture`: truncating by the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured, spilling the full output to disk while the message keeps only the leading fragment and a file pointer.
- **Upgraded norma to v0.4.3**: the harness layer now uniformly executes `CaptureOnce`, so no tool (including ones added in the future) will blow up the context from missing truncation.

### Deployment

#### Fixed

- **Fixed the activity stream failing to connect under reverse-proxy deployments**: the SSE address was previously unconditionally assembled as `${hostname}:8787`, so in deployments reverse-proxied to 443, the page loaded from `https://domain/` while SSE was pointed at port 8787 which is not open to the public, manifesting as repeated reconnects of the activity stream. Now it is distinguished by `NODE_ENV`: production defaults to same-origin, and only `next dev` keeps `:8787` to bypass Next.js's buffering of SSE; the `NEXT_PUBLIC_SSE_BASE` override remains. README now documents reverse-proxy deployment.

### Findings

#### Fixed

- **Fixed the "By asset" view's asset list having no scrollbar when overflowing**: the outer card only had `max-height` with no definite height, so the scroll viewport's percentage height could not be resolved, and with many assets the list either burst the card or was truncated with no way to scroll. Now the height cap is applied directly to the asset tree's native scroll container, adapting to window height.

### Database

#### Fixed

- **Set an upper limit on the connection pool**: `database/sql` by default does not limit how many connections the application itself can open; when the pool has no idle connections it creates new ones unconditionally, pushing all the way up to PostgreSQL's `max_connections` (default 100) before being refused. Now it is capped at 32, and excess queries wait for an idle connection instead — under the same load this manifests as slower rather than errors; if `max_connections` was lowered, lower it accordingly.
- **Improved connection reuse**: the default `MaxIdleConns` is 2, so connections beyond 2 in concurrency are closed after use and the next request must redo the TCP and PostgreSQL authentication handshake. Now the idle cap is raised to match the total cap, with a 30-minute connection lifetime and 5-minute idle eviction added, letting the pool naturally replace long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task List

#### Added

- **Task list gains a "Vulnerabilities" column**: shows counts bucketed by severity as Critical / High / Medium / Low, with non-zero buckets colored by severity, giving an at-a-glance view of each task's vulnerability volume and distribution.

#### Changed

- **The "Description / Goal" columns are narrowed**: overly long content is truncated with ellipsis, and hovering shows the full text, reducing the horizontal space long text takes from the list.

### Exploration Graph and Planning Status Overview

#### Changed

- **Streamlined the planning status overview (graph_overview) and set count caps for each list**: recent facts, finished intents, pending intents, cold-zone summaries, and confirmed vulnerability details all changed to "latest window + count fallback", with unshown items queryable on demand; this significantly reduces per-round LLM context size and avoids context bloat on long tasks (cold zones in linked task overviews are capped as well).

#### Fixed

- **After collapsing cold nodes, removed the reverse duplicate edge between digest and finding in the exploration graph**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset Interception

#### Added

- **Added global asset interception rules (blacklist)**: supports entering exact and fuzzy-matched domains / IPs / URLs as well as CIDR ranges, with CRUD and enable/disable for rules, managed on the page at "System → Asset Interception"; by default fuzzy-intercepts government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) websites.
- **Asset interception integrated into the execution chain**: before the Agent dispatches intents (`add_intent`) or inserts assets (`insert_assets`), it first performs an interception judgment on the target asset — intents that hit interception are not dispatched and assets that hit interception are not inserted, with asset info and the interception reason returned to the Agent.
- **Added task-level interception / allow (whitelist) rules**: independent of global rules and effective only for this task, with the judgment order "intercept first, then allow" — hitting interception means forbidden; if not intercepted but this task has allow rules configured and none match, then "testing not allowed"; when no allow rules are configured the whitelist is not enabled. They can be entered at task creation, or added/removed/modified and enabled/disabled in the task details "Overview".
- **Task templates support preset category and task-level interception/allow rules**: a template can save the task category and a set of task-level rules, and applying the template brings them into the new task form.

### Operation Review

#### Fixed

- **Tightened the model judge output protocol to reduce false approvals from truncation**: lowered the verdict explanation (comment) cap from 500 to 120 characters and enforced "output JSON only, with no preamble or code block", avoiding verdicts being truncated by `MaxTokens`, unparseable, and then approved via the model-failure policy (fail-open).

### Task Archiving

#### Fixed

- **Archiving now skips symlinks instead of failing the whole package**: the archive format supports only regular files and directories end to end; previously any symlink in the working directory would fail the entire task archive. Now the symlink is skipped and logged, with the remaining files archived normally (links not followed, never escaping the directory tree).

### Account and Compliance

#### Added

- **Added a "Usage notice and disclaimer" dialog before login**: login is allowed only after checking the consent box.

### License and Dependencies

#### Changed

- **The project adopts the AGPL-3.0 open-source license**, and README's license and disclaimer notes were improved.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration Chain Broadcast Board

#### Added

- **Task details page gains the "Exploration Chain Broadcast Board"** (#144): presents the task's exploration nodes (start/goal/intent/fact/finding/hint/compaction) in a timeline feed, with type filtering, keyword search, ascending/descending order, pagination, and auto-refresh; while on the "newest-first first page" it is a live position refreshed every round, and leaving that position only accumulates an unread count without disturbing current reading, with a one-click "N new broadcasts · Back to latest" return. Grouped by day, so even paging through a long task you can recognize "which day this happened".
- **Each broadcast row shows the node id** (#145), making it easy to cross-reference the exploration chain graph and locate specific nodes.
- **Broadcast node detail expansion shows upstream/downstream and anchored assets** (#147): expanding a broadcast reveals the node's upstream (where it came from) / downstream (what it produced) relationships; hovering over related entries pops up the corresponding node's card (type/status/source/time/summary/payload fragment); and lists the assets anchored to that node (type tag + recognizable text). Related data is delivered with the broadcast page, so expanding makes no extra requests.
- **Broadcast board search supports filtering by node id** (#150): in addition to "content / source", the search box gains node id matching; entering a plain number or the displayed "#41" form precisely locates the corresponding node.

### Intent Management

#### Added

- **Intent deletion supports "soft delete / hard delete" modes** (#149): pending, running, and paused intents can all be deleted; deletion requires a reason; the deletion mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is marked "deleted" and the reason recorded in a separate field, preserving the intent node and all its outputs and lineage.
  - **Hard delete**: physically removes the intent along with the exclusively-supported descendant nodes that "only it supports" (cascading along the output/intent chain to the leaves), avoiding orphaned data; the deleted intent's token metering is archived by original date; shared nodes (still referenced by other intents), goals, and task root facts are all kept, and the hard-delete dialog shows the estimated number of cascade-affected nodes.

  Both modes notify the planner of "this intent was deleted by the user + reason" and replan accordingly.

### Operation Review

#### Added

- **Approval records support filtering by status and judgment source** (#139): approval records can be filtered by approval status and judgment source to quickly locate the target record.

#### Fixed

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by history list pagination and remain fully visible while browsing history.

### Agent

#### Changed

- **Worker role description changed to a generic cybersecurity platform wording** (#138).

#### Fixed

- **Fixed cold-node compaction never triggering**: connected the Compactor to the per-task planner; cold-node compaction (cold-digest) previously never ran because it was not wired in, and is now restored.

### Chat

#### Added

- **Chat supports @-mentions of multiple record types with scroll pagination** (#135): multiple types of records can be @-referenced in chat, and mention candidates load with scroll pagination.

#### Fixed

- **Long message bubbles constrained within the conversation panel** (#137): overly long message bubbles no longer overflow the conversation area.
- **Fixed "missing upload file" when uploading with no conversation created yet**: the Composer's file picker now snapshots `FileList` into an array before clearing the input, then calls back; previously the draft state had to asynchronously create the conversation first, and when execution resumed the `FileList` live-bound to the input had already been cleared, causing the upload to lack the file field and the backend to return 400.

### Traffic

#### Fixed

- **Traffic recording proxy listens only on 127.0.0.1 by default** (#129, #130): avoids the open proxy being exposed on other network interfaces under the default configuration.

### Web

#### Fixed

- **Fixed the static-export task list page missing the global header**.
- **Completed the demo vulnerability details page's linked traffic mock, fixing the white screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation Review

#### Added

- **Approval records support source location** (#125): clicking the "source" of an approval record jumps back to the tool execution that triggered that approval — opens the original full conversation, auto-pages to the target position, expands the command and result in place, and center-highlights it in the scroll area while surrounding messages stay readable; auto-correction stops when the user scrolls manually. Across regular conversations, task Workers, planners, and main Agent segmented sessions, precise location is achieved via the persisted `tool_use_id` and task mapping (a new in-scope call ID index); when missing, duplicated, or ambiguously associated, it explicitly prompts instead of jumping to a different execution. A clear prompt is shown when the conversation was deleted or the task archive/record is missing.
- **Approval records support pagination** (#115).

#### Changed

- **Streamlined model review input** (#124, #125): the model review (LLM judge after rules miss) input is narrowed to a versioned JSON of "this one complete tool call + clearly selected brief background + local working directory". The background only takes real user messages (the current user message of the chat/task main Agent); Workers no longer attach intent summaries and also clear background inherited from a parent Agent; planners and auto-triggered conversations do not fabricate user messages. No longer attached: task description, goals, operation constraints, global exploration status, historical calls, or the Worker's full intent — these still follow the original logic for Agent execution and independent session auditing, they just no longer enter operation review. All verdicts must output JSON `decision`/`comment`, and the explanation must include "the actual operation, the consequence if it succeeds, and the matched rule"; instructions in parameters or background cannot change the review policy. The actual input snapshot sent to the model plus its fingerprint are saved; old-version snapshots are kept and version-tagged, and old inputs are not backfilled with current data.

#### Fixed

- **Model verdicts wrapped in code blocks are no longer silently approved** (#126): if the model's returned verdict JSON was wrapped in ``` code blocks, the previous parse failure would be silently approved via the model-failure policy; now the code fences are stripped before parsing, and only if parsing still fails is the configured failure policy applied.

### Agent

#### Added

- **Added experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature that can be enabled in system settings, off by default. When enabled, noa (model-driven context compaction) takes over context compaction for the four Agent types — main Agent, planner, Worker, and chat — replacing the built-in compaction; the compacted source is persisted and archived under `<workDir>/noa/<session ID>/` (not scattered across task directories), globally unique per session ID. If integration fails, it automatically falls back to built-in compaction without interrupting real tasks; the toggle is read once per run, so switching only affects runs started afterwards.

#### Fixed

- **Exploration graph tools now refuse instead of nil-crashing when there is no task context**: calling exploration-graph tools in a non-task context returns a clear error instead of crashing from nil-store dereference.

### MCP

#### Added

- **Support legacy SSE MCP services** (#117): compatible with MCP servers that only offer the old-style SSE transport.

### Traffic

#### Fixed

- **Traffic search matches record hosts port-aware** (#114): `traffic_search` host matching now includes the port, avoiding cross-talk between same-host records on different ports.
- **A `traffic_search` description migration error no longer interrupts subsequent reporter migrations**: a single migration failure is isolated and does not affect later migrations.

### Web

#### Changed

- **Task retest vulnerability options gain a divider** (#122): separators are added between retest vulnerability options for clearer visuals.

#### Fixed

- **Fixed the demo task details page crashing entirely**: in mock mode, the task details "Conversations" tab would fetch `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, so the read fallback returned `[]` based on "path ending in s means a collection", making `data.items` `undefined`, and the side hook's `merge()` threw `TypeError: t is not iterable` when iterating it; this exception occurred inside `setItems`'s updater, was re-thrown by React in the render phase, and the caller's `catch` could not catch it, so the whole page was taken over by the error boundary showing "This page couldn't load". Now the mock handler explicitly returns an empty side history, and `sideAPI.history` also normalizes the return value (`items` non-array always falls back to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **Network search gains the DeepSeek official source**: directly reuses the currently active LLM config. It differs in nature from the other three sources — DeepSeek has no directly callable search API; search only exists inside its Anthropic-compatible interface (`web_search_20250305` server-side tool), executed by DeepSeek's server. Therefore it **only supports the DeepSeek official endpoint + anthropic protocol** (OpenAI protocol endpoints will outright reject the server-side tool), and each search **additionally consumes one model call**, requests **do not go through the search egress proxy**, and are **not recorded in traffic**; results contain **only titles and links** (no summary; fetched via WebFetch when body text is needed). The settings page documents these limitations but does **not validate/block them**; whether they are satisfied is up to the user to confirm, and the "Test search" button can run a real test to verify.

### Agent

#### Added

- **workers gain cross-work lookback**: added `search_all_worker_traces` (no need to know intent_id first; globally search all work execution traces of this task by keyword for matching steps) and `get_worker_trace` (after pinning a work, list its step stream, search by keyword in place, and fetch full content by step_id), for reusing observations other work saw but never wrote into fact, avoiding duplicated effort.
- **`node_detail` delegated to workers**: together with the lookback tools above, after a worker gets intent_id / node id it can directly query that node's full detail.
- **Planning rounds triggered by `add_hint` are explicitly broadcast**: previously a new hint only folded the hint into the status overview for the planner to discover on its own; now one `add_hint` records a "human added N strategic hints: …" trigger for the planner (batched as one, not spamming one per hint), and the planner is explicitly told "this round was triggered by newly added hints" and directly sees the hint content.

#### Changed

- **Global status hints reworded to a "loosened" tone**: encouraging divergent exploration and timely reporting of cross-intent leads rather than premature convergence.
- **Streamlined worker boundary wording**: an initial block does not mean it has been fully explored; focus on "exhausting the bypass approaches within this intent before concluding".
- `insert_assets` removes the per-asset `related` parameter: its only purpose was deciding whether to add the asset to this task's scope, but the value was never persisted (re-registering the same asset invalidated it, and the UI couldn't show which were judged unrelated), so in practice it only made the model do an extra judgment that was never kept.
- **Test scope (`task_scope`) no longer affected by the coverage toggle**: `insert_assets`'s auto-add-to-scope (`source='auto'`) runs whether or not coverage is enabled, and `add_task_scope` is always provided to the planner / in-task main Agent / goal-decomposition Agent. Scope is the task's authorization boundary and the filtering basis for asset queries; the coverage toggle only decides whether to use it as the denominator for metrics, not whether to accumulate the scope itself. Previously disabling coverage disabled both the `auto`/`agent` write paths, leaving `task_scope` with only rows manually added in the UI. `list_untested_assets` still hides when coverage is off (it is itself a purely coverage-view tool).

#### Fixed

- **Fixed asset cross-contamination between tasks** (#59): `list_assets` previously hardcoded the task id to 0 and queried the entire shared asset store raw, and the model had no way to write a scope filter, so assets of other tasks (especially IPs) were taken as this task's targets, skewing the testing direction. Now it only returns assets within the test scope (`task_scope`) of [this task and directly linked tasks], matching by **ownership** rather than literal value: having a root domain in scope lets you find all its subdomains/services/endpoints, and having a subnet lets you find the hosts and services in it; fetching out-of-scope assets by id also returns nothing. Non-task contexts (Auto/pentest) have no scope to rely on and still fall back to the full store. Also fixed the ownership blind spot where IP-direct hosts (e.g. `http://1.2.3.4/api`) could not be matched by a subnet scope. The UI's "Test assets" view (filtered by task producer) is unaffected.
- **Fixed workers' cross-work lookback tools being mistakenly removed**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker default toolset, an old "shrink worker tool surface" migration unbound them again at startup, so workers actually never got these tools. They have been removed from that unbinding list, and databases that already ran the old migration get a one-time re-binding back to workers.
- **Fixed occasional DB write failure for `/btw` side questions**: the side_question checkpoint now strips NUL (`\u0000`) escapes unsupported by JSONB before persisting, avoiding write errors for content containing that character.

### Triggers

#### Fixed

- **Merged trigger conversations deduplicate task description/goal per task**: when multiple tasks trigger a merge, the same task's description/goal is no longer concatenated repeatedly into the message, avoiding long goals stacking up and blowing up the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main Agent supports **multiple conversations**: you can create, switch, and independently reset context, and each conversation is interactive.
- Supports persisted `/btw` **side questions**: doesn't interrupt the main-line follow-ups; Q&A is persisted and survives restarts.
- `spawn_task` gains `source_task_ids`, letting new tasks **read-only inherit** the source tasks' assets and findings.

#### Changed

- Disabled workers' cross-engagement memory and shrank their default tool surface: reading context and cross-work review are planning responsibilities; a worker only handles execution and write-back of a single intent.
- Streamlined the worker default prompt; `get_worker_output` removes the `terminated` and `worker_name` fields; streamlined the descriptions of `insert_assets` / `list_assets`.

#### Fixed

- Fixed work being cut short by **idle rounds**: the model sometimes spends an entire round outputting only thinking, giving neither body text nor tool calls, and the harness sees this as a natural end (`end_turn` with no `tool_use`) and finishes directly with `completed` + empty summary — an unfinished intent stops halfway, appearing as "task ended normally but with no text summary". None of the five LLM retry layers could catch it: it is not an error; the same-provider safe-window retry's entry is "stream failure", the circuit breaker judges success when `err == nil`, and intent rerun only recognizes `model_error`; the SDK's empty-response retry also can't catch it, because that layer judges emptiness by "whether any event was yielded", and thinking increments are themselves events. Now the work's Stop hook recognizes such rounds and injects a continue instruction, letting the model proceed to the next step with the thinking it already produced — deliberately not choosing "resend as-is", because this kind of idle is usually determined by the prompt and context shape, a stable behavior rather than random flakiness, and resending would only make the model think again.
- The count reuses the **empty-response retry count** from the LLM page's "Retry and backoff" (both answer "the model finished without producing substantive content", just with different criteria and means), defaulting to 2; setting `-1` disables it and reverts to the old "idle round ends the work" behavior. It is a **total** cap for one intent, not a consecutive count: the harness itself already limits consecutive idles to only one push, and the quota only refreshes after a real tool round, so this number blocks loops like "tool → idle → push → tool → idle" from draining the intent budget. When triggered, the log records `[work <worker> · #<intent>] idle round… injecting continue instruction (n/N)`. See the design in `docs/LLM-retry-design.md` §1.1.
- Fixed the context budget and input layout of `/btw` in long conversations; in non-secure contexts (non-HTTPS access) the side-request ID generation is degraded.

### Traffic

#### Added

- Findings support **linking multiple traffic evidence items**, sortable, with notes, and distinguished by role (request/response/corroboration).
- The report Agent **automatically links** related traffic evidence before writing the report.
- Added an Agent **traffic binding toggle**, and completed evidence handoff between Agents.

### Tasks

#### Added

- Supports **conversation-level finding retest**: retests run in an independent Agent conversation, and lists and details show the running status.

### Interception

#### Added

- Approval records gain **detail and execution audit**: you can view the tool request context, model/rule initial judgment, execution output, and parameter fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the issue where the test connection did not re-send the custom session header, causing opencode zen to return 400.

### Network

#### Added

- MCP's HTTP transport supports **skipping TLS certificate verification**, making it easy to connect to services with self-signed certificates.

### UI

#### Added

- The conversation list is grouped by Agent, with expand/collapse and independent pinning; conversations support filtering by Agent.
- The attack chain graph renders compressed (digest) nodes and collapses their members.

#### Fixed

- Fixed the white screen caused by out-of-sync login credentials.
- The interception prompt text now clearly marks "platform control", avoiding misreading as target-side defense.

### Deployment and Updates

#### Added

- Supports **one-click update from the page**: the system config page gains a "Version and updates" card, and the top bar shows a hint when a new version is available. The flow is: download the release package → verify `SHA256SUMS` → smoke test → stage → exit and let the daemon script relaunch to complete the swap, with the page auto-refreshing.
- Added daemon start scripts `start.sh` / `start.bat` as the official startup entry (both the release package and Docker image include them; `install.sh` unchanged), deciding whether to relaunch based on the exit code and forwarding SIGTERM to artex. Verification and swap logic are all in Go, keeping the scripts foolproof.
- Automatic failure fallback: if verification or smoke fails, discard and keep running the current version; if the new version fails to start 3 times in a row, roll back to the previous version. The settings page also has manual rollback (note: the database schema will not roll back).
- Updates only accept the GitHub domain and enforce HTTPS; the release source is not configurable; development builds disable one-click update. GitHub query results are cached for 30 minutes to avoid the top-bar hint exhausting API quota.

#### Known Limitations

- Under Docker only the program is swapped, not the image: the toolchain does not upgrade along with it, and rebuilding the container reverts to the image's bundled version; use `docker compose pull artex` when needed.
- The `skills/` in the release package is not synced, so new built-in skills in a new version do not take effect automatically.
- Updating means restarting, which interrupts running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- The LLM page gains a "Retry and backoff" tab: the **count** and **interval** of all five retry layers are configurable. A single model call's failure passes, from inside out, through five layers: connection retry (SDK, connection reset / timeout / 429 / 5xx before the stream starts), empty-response retry (SDK, ended normally with no content, openai format only), same-provider safe-window retry (stream replays before any output is delivered to the caller), polling circuit breaker (cools down and skips after consecutive failures hit a threshold), and intent rerun (the whole intent reruns after the worker ends with model_error) — the outer layer only runs after the inner layer is exhausted. Each layer has two knobs with unified semantics: empty = use the original default count and exponential backoff; fill the count to use that count; fill the interval to replace exponential backoff with a fixed interval; fill -1 = disable that retry layer. The first three layers follow the endpoint and can be overridden field-by-field in each model config over the global default (pinning only the interval still inherits the global count); the circuit breaker and intent rerun are process-level semantics with only one global copy. Changes take effect hot after saving, no restart needed. Leaving everything empty means current behavior; old databases stay byte-for-byte unchanged after upgrade (new columns default to 0, and absent settings keys mean all defaults). The connection test deliberately omits these parameters — it has a 30s hard timeout, and stacking the user's configured retries would only turn usable endpoints into timeout failures. See the design in `docs/LLM-retry-design.md`.
- Each LLM config supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries this HTTP header, with the value being the current running session id (chat conversations as `conv-<id>`, workers as `exp<x>-worker-i<intent>`, etc.), for gateways that do prompt caching / sticky routing based on a session-id header. Implementation reads the session id from the request context and injects it, no changes to norma needed, and the same shared provider can emit different header values per conversation. Old databases get a migration, and the frontend LLM config dialog gains an input box.

#### Fixed

- Fixed saving an LLM config directly erroring with `23502` when the session header field is empty: the column is `NOT NULL DEFAULT ''`, and previously the mistaken `NULLIF($n,'')` wrote unfilled session headers as NULL, triggering the not-null constraint; now parameters are passed directly per empty-string semantics.

### Agent

#### Added

- Wall-clock timeout changed to **finish in place**: relying on norma v0.3.6, when `MaxDuration` expires it interrupts the running tool and continues on the live ctx for the wrap-up rounds (writing back identified content + summary, with the terminal state timeout), no longer relying on each worker/planner's `maxDur+90s` external hard ctx that killed stuck runs as `aborted_tools`. Chat uses the same harness and automatically gains the same behavior; mainagent has no `MaxDuration` and is unaffected.
- Stagnation fallback: when the planner wakes on heartbeat / no-change and the graph has no open or running intents at all, its opening message becomes a stagnation warning, clearly stating that no worker is running and no direction is queued, and this round must produce one or more mutually non-duplicate new intents (producing 0 intents is not allowed).

#### Changed

- Worker prompt refactor: the intent / launch instruction / anchored asset raw JSON moves into the system prompt, re-assembled every round and never squeezed out by compaction, and reruns no longer depend on the transcript's first message being retained; the startup user message is slimmed to only the global status overview (degradable, tolerant of stale). The cost is system mixing in per-intent data and losing cross-intent cache reuse — a deliberate trade-off for "not losing the intent".
- Streamlined the planner / worker default body text and fixed several practical issues: the planner distinguishes `recent_done` by state (for blocked/exhausted, check the trace first before deciding — neither treat as dead ends nor blindly rerun), negative conclusions become "observation / doubtful, not definitive" and evidence is checked before acceptance, and when the goal is unmet with no open/running intents it must produce (hard floor), with depth prioritized over coverage; workers write only "observation + tentative reading" for negative conclusions, the judgment belongs to the planner, and cross-intent leads go into the fact's summary for the planner rather than being chased by the worker. reseed appends the new defaults as a new version and switches to it, while user customizations / old versions stay in history and can be rolled back.
- Shrunk the work agent's default toolset and asset write-back prompts: a worker only handles execution and write-back of a single intent, and reading context / cross-work review are planning responsibilities, so its default tools remove `list_facts` / `node_detail` / `list_companies` and the cross-work searches (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`), keeping only `list_findings` (dedup before reporting a finding) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt removes stale field notes that conflict with the `insert_assets` schema (`type=tech`/`on_url`/`props`). Old databases get a one-time migration stripping the corresponding bindings, while planner/main bindings of the same name are untouched.

### Exploration Graph

#### Added

- **Exploration graph cold-node compaction (cold-digest)**: folds old and long-inactive intents / facts into digest nodes shown in `graph_overview`; the original nodes are kept permanently and fully restorable by id (lossless storage, presentation-only compression, reversible folding). Hot/cold determination uses reverse reachability + any live branch counts as hot, with R=6 rounds of debounce and connected-component grouping; background compaction (minor folds uncovered cold blocks, major re-sources and re-compresses to merge fragments) has activity recheck and cooldown mutual exclusion, never touching the hot path and never overwriting revived nodes. The overview side provides `cold_digests` and an asset-indexed `cold_index`, with `expand_digest` / `expand_index` responsible for restoration. Overviews of linked / inherited tasks reuse their own folded view, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are given only to the planner and main agent, not workers. All come with old-database migrations.
- `graph_overview` outputs `finding_list` in full: findings are the task's highest-value products and usually few per task, so they are now carried in full in the overview (unlike facts, which only get a recent window), letting the planner/worker see all confirmed vulnerabilities at a glance every round without calling `list_findings`. Each entry is trimmed to `{id, summary, evidence?, from_intent?, assets?}`, with affected assets given readable content (url / domain / ip:port) rather than bare ids.

#### Changed

- `graph_overview` no longer flattens `hosts`: the coverage block removes the host list (in wide-scope tasks each round would carry up to 500 host strings with limited value for planning decisions), keeping only `host_count`, with specific hosts queried on demand via `list_assets`. Added a top-level `done_intents_total` (total finished intents), parallel to the ≤15-truncated `recent_done_intents`, so the planner knows whether dedup was truncated.

### Tools

#### Changed

- `add_company_scope` default binding changed from worker to planner: defining the company asset scope is the responsibility of planning / main control / Auto; workers only execute exploration. New databases seed the default binding as mainagent/planner/auto, and old databases get a one-time migration.
- planner default-binds `list_assets`: it now has both `list_assets` (DSL full-store search) and `list_untested_assets` (in-scope untested); old databases get a one-time backfill that does not override user unbindings.

### Tasks

#### Added

- Task list gains a "Running Workers" column: counts the intent nodes under the task with `state='running'`, with the same definition as the "Running Workers" in the task details page overview, showing 0 when there are no running workers.

### Network

#### Added

- Added a **global egress proxy** config: all target traffic can egress through a unified global proxy (http/https/socks5, supporting `user:pass`). When traffic capture is enabled, it acts as the upstream of the MITM recording proxy (traffic is still recorded, then egresses through the proxy, with both the interception and passthrough paths going upstream without leaking the source IP); when capture is disabled, it is directly injected into the agent's bash environment and WebFetch (`proxyEnv` gains `ALL_PROXY` support for socks5). The config is stored in the settings KV table (no migration needed), and the frontend system config page gains a "Global proxy" card, independent of the network search proxy and LLM proxy.

### UI

#### Fixed

- Fixed semantic errors in intent status labels: `exhausted` "exhausted" → "budget exhausted" (actually the step/time budget was cut off midway with only partial results written back, not that the direction was fully explored), `blocked` "intercepted" → "execution error" (actually model / API / network failure retries exhausted with the intent basically never truly explored, not target / WAF interception), and added the previously missing `stopped` "stopped" (a work manually stopped by the user).

### Dependencies

#### Changed

- Upgraded norma to v0.3.4: MCP tool output gains truncation and disk spilling (see `651b961`); the later v0.3.6 supports wall-clock finish-in-place.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model config gains an "output cap", plus a choice of which request field name the cap uses: the cap limits how many tokens a single reply generates at most, sent with every request, and 0 (default) = don't send the field, leaving it to the server default; it is different from the "context window" — the latter is the model's total capacity, used only locally to compute the compaction threshold and never appears in requests. The field-name toggle only matters for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which most compatible gateways only recognize; OpenAI's official reasoning models (o series / GPT-5) conversely only recognize `max_completion_tokens` and will error with `unsupported_parameter` when receiving `max_tokens`, so such endpoints need manually switching to the new field. The two formats each fix the field name (Anthropic uses `max_tokens`, Responses API uses `max_output_tokens`), so for non-openai formats the toggle is greyed out and cleared on save. Also wired up a link that had never been connected: none of the five places — planner / worker / chat / main Agent / goal decomposition — ever set an output cap; openai systems simply never sent it and Anthropic went through the SDK's 8192 fallback, so merely adding a config item without wiring this line meant filling in a number had no effect; now the value is resolved per round from the config, just like "streaming output", taking effect the round after a failover switch. Behavior is completely unchanged after old-database upgrade (new columns default to 0 and empty field name).
- When clicking "Test" in the backend, each HTTP attempt's status code and the gateway's raw response body are written to the server log (response body truncated to 4K), making it easy to diagnose 401s, quota text, empty frames, returned HTML pages, etc., instead of only seeing the UI-collapsed ok / err. This logging does not depend on the "LLM records" toggle.

#### Changed

- The task LLM config chain can now be modified in any state, no longer restricted to "running / paused / config chain exhausted": after a task ends (done / failed / timeout), the main Agent conversation still uses this chain, and when a chain model had problems it previously could not be changed, making further interaction impossible. Both the backend HTTP and DB transaction final-state blocks are removed, and the frontend dialog also allows editing and saving for final-state tasks. Saving a final-state task no longer reopens quota-blocking intents (those intents would be moved to open, with no worker to execute them and no longer meeting the "rerun intent" condition); to keep running, still use rerun intent / add goal, which pull the task back into the running state.

### Agent

#### Changed

- "Send a message to a running Worker to steer direction" now reuses the existing pause / resume + transcript resume mechanism, behaving consistently with the main Agent conversation: previously it used a self-built intervention persistence protocol that touched the scheduling barrier, resume flow, and more than a dozen activity-query filters. Now messages are injected via the next round's input, and the intent runs directly in a dedicated goroutine, unrestricted by the 3-slot worker pool and running as soon as sent; the frontend keeps the message box and restores the "continue directly" button, with sending carried by SSE. No DB schema changes. Trade-off: messages live only in memory with no crash recovery, and when sending a message the task may momentarily exceed concurrency by one worker (a low-frequency scenario, acceptable).

### Tasks

#### Fixed

- Fixed out-of-memory (OOM) crashes when archiving large tasks: archiving now writes the snapshot streaming, no longer reading the entire task into memory at once; also closed three recovery gaps on the cold-archive path — in modern installs traffic only exists in SQLite, and previously a crash between the PostgreSQL commit and the SQLite commit would leave the archived task's traffic stranded in hot storage with no way to recover; now a staging log is written whether or not a history directory exists, and a missing `journal.json` is treated as discardable.
- Fixed system lag when there are many tasks: loading conversations and loading the interface had noticeable delays. Queries for the task list, task context, and exploration records were optimized together, and the frontend dashboard, conversation page, and task details page reduced duplicate requests in sync.

### Assets

#### Changed

- Removed the "max 256 rules" limit on company asset scope: companies entering scope one IP / domain at a time easily hit this cap, after which they could only split into multiple companies, while the scope itself is not slower with more entries. The limit was removed in all three places — frontend, backend, and demo mock — while a single rule is still limited to 1024 characters and the 2 MiB request body cap remains as a fallback (roughly forty to fifty thousand rules).

### Skills

#### Fixed

- Fixed uploading a skill archive erroring with "upload failed: zip: unsupported compression": the Go standard library only includes two decompressors, Store / Deflate, so bzip2 and Zstandard archives written by compression software at non-default settings could not be decompressed at all. Now those two decompressors are added (pure Go, no new external dependencies); formats that genuinely cannot be decompressed — Deflate64 / LZMA / XZ / PPMd and the like — as well as encrypted archives, now report a localized prompt before decompression, directly naming which file used which compression method and how to re-pack, instead of dumping the underlying English error on the user.
- Fixed filename validation inside skills judging non-ASCII filenames as illegal: the path validation was previously an ASCII whitelist of `[A-Za-z0-9-_./]`, so a single non-ASCII-named file in the archive (like `reference/notes.md`) would fail the whole upload with "archive contains illegal path". Changed to a Unicode blacklist: filenames in any language and spaces are allowed, while still rejecting control characters, illegal UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments, with directory-traversal protection unchanged. Skill names are likewise relaxed — ASCII still limited to lowercase letters, digits, and hyphens (the agentskills.io spec), while non-ASCII letters like Chinese can be used directly as skill names, but spaces, dots, and path separators are not accepted.
- Fixed garbled filenames or whole-package rejection when decompressing non-ASCII archives made by Windows compression software: such zips don't set the UTF-8 flag and write filenames in GBK, so now GBK fallback decoding is applied before path validation. Also, quoted frontmatter like `name: "Chinese Skill"` now correctly extracts the skill name.

### UI

#### Added

- Findings page gains a flat "All findings" view set as default, switched with the original "Grouped by task" view via a page-header Tab: the grouped view requires expanding task by task to see vulnerabilities, which is a detour when you just want to scan a cross-task list. The flat view is one large cross-task table (10/20/50/100 per page) behaving identically to the grouped view — check to export, expand rows to see evidence and detailed report, edit name/category/severity inline, change disposition status, drill down, delete. Both views share the statistics cards and filter bar; switching doesn't lose the filter, and the current view plus filters are remembered locally; polling only hits the current view, and inline edits write to both caches so switching over never shows stale data.
- Findings page gains a "By asset" view: the left side is an asset tree (company → root domain / IP → subdomain → service → endpoint, with the company layer appearing only when assets actually have an owner), and the right side shows the findings under the entire subtree of the selected node, sharing the same table and filter set as the other two views. The tree only includes assets that have findings, with ancestor chains filled in as needed (when a finding hangs only on the deepest endpoint, the layers above can still be assembled), and node counts are subtree aggregates deduplicated by finding; findings whose asset was deleted or that had no associated asset go into the "Unassociated assets" bucket. Unlike the other two views, the asset view does not poll — it queries only when entering the view, changing filters, adding/editing/deleting findings in the page, or clicking the refresh button on the tree; the left tree is a navigation structure with no need to recompute every 5 seconds. Each tree layer shows only its increment relative to the layer above (subdomains drop the root domain suffix, services show `https :443`, endpoints show only the path), with full values in hover tips and breadcrumbs; when there are too many nodes, the endpoint/service layer is dropped entirely with a notice, while counts still roll up to the parent.

#### Fixed

- Fixed squeezed conversation records being unreadable in mobile conversation details: on narrow screens the conversation list collapses, giving the width to the records themselves.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: added `Config.MaxTokensField`, letting OpenAI Chat Completions' output cap be sent as `max_completion_tokens` (reasoning models only recognize this key). The two keys are mutually exclusive and only one is sent; the default is still `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security alerts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "non-streaming" toggle reverting to "streaming" after saving and reopening (#69): the config list endpoint's DTO was missing the `streaming` field and the response never returned it, so the frontend read `undefined` and always fell back to the default streaming; the actual write and DB storage were fine, it just couldn't be read back. The DTO now returns the field (without `omitempty`, so `false` must appear in the response).
- The test connection now verifies the model actually replied, and tests using the config's real send/receive mode (#65): previously it only judged HTTP success, so a request that connected but with the model replying nothing (thinking burned the budget / body text swallowed by safety policy / compatibility layer dropping `content`) still reported "connection successful", contradicting the "no reply" behavior in conversations; and the test always went streaming, so a non-streaming config was actually testing a different channel. Now an empty reply is judged as failure, the model's reply is shown in the prompt on success, and the current streaming toggle is carried into the test, keeping "test passes" consistent with "conversation actually works".

### Agent

#### Changed

- `list_facts` changed to pagination + keyword filtering to avoid a single call blowing up the context when there are many facts (#74): default returns the latest 20, supporting `limit` (cap 100) / `before` cursor / `q` summary keyword, returning `{facts, total, has_more, next_before}`; an overly long single summary is truncated by length (full text still via `node_detail`). Worker / planner prompts are synced to pagination semantics. Old databases get a one-time migration flushing the new parameter schema into the tool catalog table (`SeedTool` first-insert only, otherwise the tool management page shows "no parameters").

### Tools

#### Added

- The "Tool execution" page gains tool call count statistics (#72): the toolbar's "Statistics" button opens a dialog showing, per tool, call count, share, and failure count (descending by count); it reuses the list's task / keyword filters and statistics the entire result set rather than the current page, fetching only when the dialog opens.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixed silently dropped reasoning/refusal in OpenAI-family responses (Responses now includes `reasoning_text`), dedup of the three reasoning field names, bounded retry of empty responses (zero content blocks), and gateway 400s caused by compaction boundaries cutting tool pairs (also fixing existing orphans already baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM config supports streaming / non-streaming switching (default streaming): on goes through streaming SSE; off goes through true non-streaming (`stream:false`, one-shot full JSON return), which can bypass some gateways' poor SSE implementations (empty frames, dropped thinking-field frames), at the cost of losing real-time progress and real-time token counting during the run. worker / planner / mainagent / chat / goals all dynamically take the value from the currently active config; the `llmpool` / `llmrec` / task-runtime three-layer Provider wrappers are all non-streaming compatible; `llm_profiles` gains a `streaming` column with an `ALTER` backfill for old databases (default `true`, transparent to old configs).
- Supports OpenAI Responses API format LLM configs: each config gains a third format `openai-responses` (calls `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` normalized, default model `gpt-5`; the `format` constraint in `llm_profiles` adds `openai-responses` with an idempotent migration backfilling old databases; the frontend format dropdown gains "OpenAI (Responses API)". Dependency upgraded to norma v0.3.1 (including the `reasoning_content` return fix).
- Records raw HTTP request/response for LLM calls: captures the real wire body at the HTTP transport layer, preserving tool schema, `tool_use` blocks, and raw SSE frames that the normalized view can't show; norma's internal retry attempts are each preserved; `llm_records` gains `raw_request` / `raw_response` columns with an `ALTER` backfill for old databases; the recording page's detail panel gains a "raw" view toggle and request/response copy buttons (compatible with the `execCommand` fallback in non-secure contexts).

### Conversations

#### Changed

- Conversation list multi-select becomes a "multi-select" mode toggle: the default list no longer keeps a per-row checkbox (cleaner look), and the header becomes a "N total + multi-select" button; clicking "multi-select" enters selection mode (checkboxes, select all, batch delete), "Done" exits and clears the selection, and a fully successful batch delete auto-exits (with failures it stays in selection mode for retry); per-item rename / pin / delete still use each row's ⋯ menu.

### UI

#### Changed

- Enhanced task management, conversation operations, and traffic viewing experience (#57): task list column-sort preferences persist; inline rename now triggered directly by an icon (no longer via menu); drawer (sheet) interactions and traffic viewing details optimized.

#### Fixed

- Worker asset tags show only domain and IP, and correctly handle empty-tag scenarios.

### Agent

#### Added

- Planner gains a "quantitative acceptance check" when judging goals: when a goal contains quantifiable acceptance criteria (asset test coverage reaching X%, obtaining N flags, gaining a certain permission), the planner must check `graph_overview`'s measured values (`coverage.pct`, findings count, etc.) before `prove_goal`; if the measured values fall short, `prove_goal` is forbidden and intents are dispatched to close the gap, and it may not mark met early on grounds of "mostly complete / broadly achieved". Fixed the problem where a goal requiring 100% coverage was judged complete with only 40% measured.

#### Changed

- Rewrote the "0 intents this round" determination basis in the planner prompt: the original text described 0 intents as "the most common and most important principle", which could make the planner give up too early when the goal is unmet and untested surface remains in scope. Now 0 intents should only be produced in two specific situations — ① every direction thought of is already covered by open/running/recent_done intents; ② the next step depends on the output of a currently running work, and the output has not appeared yet (wait for it to finish and replan on the next wake after the graph updates). A reverse constraint is added: when there is a genuinely uncovered new direction that does not depend on running work, or the goal is unmet with untested surface remaining, don't give up just because "0 intents is common".
- Strengthened the "evidence threshold for negative conclusions" in the worker prompt: for negative conclusions like "not injectable / port closed / no login entry" that could make the planner abandon an entire direction, the worker must exhaust the reasonable approaches within the intent first (changing encoding/parameters/path/method); if approaches are not exhausted or evidence is weak, mark `confidence=inferred` to avoid welding an entire route shut with a hasty `observed` negative (wrong negatives early in a task especially skew the direction and are hard to self-heal).
- The above planner / worker default prompt changes are appended as a new version and switched to current via a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag); old versions stay in the version history, and users who customized their prompts can restore from the version records.

### Operations

#### Added

- Added `reset-password.sh` to reset the administrator (username fixed `ARTEX`) password: supports both local / docker deployments; connection info can be specified explicitly or auto-read from `--dsn`/`$ARTEX_PG_DSN`/`config.json`; generates a bcrypt hash compatible with backend login using `pgcrypto` in the database and writes it back to `settings.auth.password_hash`, requiring no service restart after reset. The password is passed via an environment variable, does not enter the process argv, and is escaped to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting multiple tasks in the task list you can batch-modify their category, with a "Uncategorized" target to move tasks out of the current category; the whole batch writes in one transaction, and failures only occur for tasks already deleted after being checked.

#### Changed

- The new task "category" changes from a dropdown to a searchable input: typing filters existing categories; pressing Enter on a name not in the store (or clicking "Create" in the dropdown) creates and selects it instantly, with the selection shown as a removable tag; still limited to a single category.

### Conversations

#### Fixed

- Fixed messages being blocked by "LLM not configured, cannot converse" when the conversation has a selected LLM config but there is no globally active config. The pre-send check previously only looked at the global active config, while actual execution prioritizes the conversation's selected config — the two resolution logics were inconsistent; now unified into one (conversation/Agent binding first, global as fallback).
- The cannot-converse prompt is now subdivided by state: with no config at all it says "add a config"; with configs but none active it says "activate one or assign a config to this conversation"; no longer uniformly showing "not configured", making diagnosis easier.

### Agent

#### Added

- For tasks whose goals are all achieved, intents dispatched by the main Agent via `add_intent` can be directly claimed and executed by a Worker, stopping once done: the Planner does not run in this case (a task with no open goals doesn't enter planning), avoiding it re-judging the goal as achieved and canceling the just-dispatched intent; after the frontier drains, the task returns to completed. Before dispatching an intent, the main Agent asks the user whether to register it as a formal goal based on its content; registering restores the task to regular autonomous planning.

#### Changed

- When `step_ids` for `get_worker_trace` / `get_task_worker_trace` exceeds the one-call cap (5), it no longer errors directly but returns the full content of the first 5 steps, and reports in the result via `returned_step_ids` / `omitted_step_ids` / `notice` which were fetched and which remain, with the hint "if sufficient, no need to fetch more". Duplicate and invalid ids passed in are deduplicated and dropped before counting.

#### Fixed

- The task list now sorts by task id descending (newest created first), replacing the original sort by creation time. Multiple tasks created at the same moment share timestamps and sort unstably, and combined with the 10-second polling and random iteration of the in-memory map, they would frequently swap positions in the list.

### Assets

#### Fixed

- Fixed company ownership recomputation being killed by a single dirty row: when `assets.ip` stored a hostname (written by the Agent or asset API), `a.ip::inet` would throw `22P02`, making adding/modifying company asset scope and deleting companies all fail and roll back. Switched to the safe cast `try_inet()`, skipping invalid values instead of aborting the whole statement.
- Unparseable `ip` values are no longer silently skipped: after saving company scope, it clearly reports how many assets have `ip` values that are not valid IPs (including specific ids and values), reminding that these assets won't be attributed by IP/CIDR rules; the same info is also written to the server log, covering paths without a frontend response (deleting companies, scopesentry sync, Agent writes).
- Task test scope IP/CIDR matching also switches to `try_inet()`, replacing the original regex guard that only checked the character set (hostnames like `abc.def` made entirely of hex letters previously still slipped through and triggered the same error).

#### Changed

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return per-item errors with `index` and directly give the fix (use `type=subdomain` with `domain`, or resolve A/AAAA records first), so the Agent can correct itself and re-insert. Other assets in the same batch are unaffected and stored normally.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- Running Workers support per-item pause, resume, and cancel; pausing preserves the intent, facts, and findings, while canceling transactionally cleans up the current intent and its direct outputs after execution exits.
- Added the `paused` intent state, an execution fence, and named termination reasons to prevent late blackboard writes after pause, cancel, or task deletion.
- Added an optional task concurrency cap; new tasks, resumes, finding drill-downs, and queue backfills all go through a persisted FIFO admission path.

#### Changed

- Worker single-run wall-clock default duration adjusted from 600 to 1200 seconds; a timed-out task re-times on its next real execution after revival.
- The real execution states of Workers, the Planner, and the main Agent uniformly drive the task status badge, fixing the issue of still showing "idle" while a Worker is running.
- Worker conversations keep per-item control and the current conversation's token summary; the model name moves to the right of the current conversation title.

#### Removed

- Removed the Worker multi-select, select-all, and batch pause/resume UI, along with the Worker batch control API and mock contract.
- Removed the token badge and tooltip from Worker list rows; the underlying complete token ledger and task aggregation APIs are kept.

### LLM

#### Added

- Tasks support an ordered LLM config chain; when a provider's quota exhaustion is clearly identified, it automatically switches to the next config and persists the current config, exhausted state, and error summary.
- Running and paused tasks support editing the full config chain, reordering, and manually switching the current config, effective from the next LLM call.
- Auto-switch, manual switch, and full-chain exhaustion are written as structured system activities and surface deduplicable alerts via the task activity stream.
- Added a task-role model resolution API, uniformly resolving the config and model the Main Agent, Planner, and Worker will use for the next call.
- Added an optional global LLM Pool supporting configurable call order, specified model failure fallback, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger aggregating input, output, and cache tokens by task, conversation, model, and config.

#### Changed

- Goal decomposition, the Planner, Workers, and the main Agent share a task-level LLM runtime (resolution priority below).
- Failover only responds to explicit quota, balance, or billing errors; ordinary rate-limiting, auth, network, server, and context errors do not cause a wrong switch.
- The LLM config page becomes a config card list with right-side drawer editing, and provides Pool polling order, priority, exclusions, health status, and recovery actions.
- Config chain exhaustion info supports line wrapping on narrow screens; the current model changes from a conversation-list icon to a text label next to the current conversation title with a full tooltip.
- The in-task per-role LLM resolution order is adjusted to "Agent binding → task config chain → global": roles explicitly bound to a model always run on that model; unbound roles fall to the task chain, and when the task chain is empty they fall to global.
- Leaving the egress proxy empty means direct connection, no longer falling back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (the explicit `ARTEX_LLM_PROXY` is unaffected); proxy input supports `socks5://user:pass@host:port` with credentials.

#### Fixed

- Transient streaming failures before any output is produced (nothing delivered yet) now retry with safe backoff on the same provider, significantly reducing the "ran only one or two tools then died, no summary, terminal `model_error`" situation; quota exhaustion, over-long context, and deterministic 4xx errors do not retry and are still handled by failover and compaction recovery respectively.

#### Removed

- Removed the icon-style LLM indicator from the conversation list to avoid confusion with Worker status icons.

### UI

#### Added

- Tasks support creating, renaming, deleting, and filtering global categories; new tasks can directly choose a category.
- Added global task template CRUD and a right-side management drawer; new tasks can load preset descriptions and goals, or save the current content as a template.
- New tasks support linking multiple source tasks and multiple company asset scopes; company scope auto-recognizes domains, URLs, IPs, CIDRs, ICP, and company keywords through a single multi-line text box.
- Task test assets support real-time add and delete, recording sources such as manual, company, inherited, or Agent-discovered; the Worker conversation title shows the current test assets and a source summary.
- The findings page groups by task with independent in-group pagination, and supports filling in descriptions for findings and creating high-priority drill-down intents.
- Conversations support rename, pin, unpin, and delete; the task list supports current-page batch pause and resume.
- Traffic details and tool execution details become right-side drawers; HTTP messages add the `Host` and highlight the request line, status code, headers, JSON, and markup body.
- The task list action column gains "Details" and "Pause/Resume" buttons; the conversation send key can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and the Web search area gains a proxy input.
- The badge to the right of the conversation title now shows the LLM config name, with the model ID moved to a hover tooltip; tasks can have an optional name, and the task list gains a name column and search (falling back to the description when empty).
- The skills page adds usage count, last-used time, and missing-dependency statistics, making it easy to locate skills that aren't active or installed.

#### Changed

- Task titles become focusable detail links; task assets, company assets, findings task groups, and in-group findings use server-side real pagination, stable sorting, and exact totals.
- Adding a company switches to a right-side drawer, with scope entry unified as multi-line text plus real-time recognition, validation, and type preview.
- The main Agent input box supports auto-grow, `Enter` to send, `Shift+Enter` for newline, and avoids accidental sends during IME composition.
- Agent preview, task reports, and related details uniformly use a shared Markdown renderer; delete confirmations, long errors, and mobile drawer width are uniformly fixed.
- The task template selector displays and searches by template name and submits by template ID; the original system font size is restored, and the app version is uniformly shown as `0.3.3`.
- Task total and current-conversation tokens only highlight the input, cache-read, and output values; the send button uses a cleaner up-arrow icon.
- The task overview's test scope shows company names instead of IDs.

#### Fixed

- Plain text containing dots is no longer misjudged as an ICP filing number.
- Fixed variable directory entries colliding with global runtime variables (like `{{.Now}}`), causing duplicate keys rendered in the Agent editor's variable list.

#### Removed

- Removed the task card's separate "enter" button; clicking the task title uniformly enters details.
- Removed the dashboard "new task" button, the new-company Logo URL input, and the vulnerability count badges at the top of the findings page and in task groups.
- Reverted the 10% global font-size increase, and removed the Worker selector, model icon, and row-level token stats from the conversation list.

### Agent

#### Added

- New tasks can directly link multiple existing tasks, read-only inheriting in real time the direct source tasks' goals, facts, findings, completed intents, asset scope, and blackboard context.
- Blackboard read tools support on-demand querying of source task nodes, facts, findings, and execution traces; inherited nodes carry source markers and all write tools refuse to modify them.
- Company scope tools support domains, URLs, IPs, CIDRs, ICP, and company keywords; keywords only serve as Agent scope hints and do not participate in automatic asset attribution.
- Finding drill-down creates a high-priority manual Worker intent in the original task with an asset anchor and a `derived_from` edge.
- The overview gains a "Goal management" card, supporting manually viewing, adding, modifying, and deleting goals; adding or modifying notifies the Planner and revives the task, while deleting hard-deletes the goal node (cascading edge and anchor deletion) without reviving.
- The main Agent gets the `steer_work` tool by default, allowing real-time corrective instruction injection into a running Worker without interrupting or losing progress (still validating that the intent belongs to this task).

#### Changed

- Source task intents do not enter the new task's frontier; the new task continues with an independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: it is stopped to `stopped`, requires a deletion reason, attaches the reason as a fact to the intent and writes it into the payload, and the planner perceives it as a `cancelled` trigger (intent content and reason preserved).
- The main Agent conversation becomes server-side activity-stream driven, with recoverable input on send failure; opening a conversation auto-scrolls to the bottom and keeps the last reply visible after detail lazy-loading.
- Pausing a task terminates the current Main Agent, Planner, and Worker calls, but does not prevent the user from actively starting a new main Agent orchestration conversation during the pause.
- Cancel, close, and stream interruption uniformly preserve the real termination reason, generated content, run rounds, duration, tokens, and unreturned tool calls.

#### Removed

- Removed the main Agent's client-side optimistic message echo to avoid duplicate messages and cross-conversation stream bleed under pause, failure, or concurrent activity.

### Constraints

#### Added

- The goal decomposition stage first extracts operation constraints (allow/deny) before decomposing goals, and the main Agent can supplement them at runtime; constraints are injected into the Planner and Worker system prompts at the highest priority, and debiased diversity and surface-expansion exploration explicitly obey the constraints.
- The overview gains a "Constraint management" card and CRUD APIs; constraint injection scope can be toggled separately for Planner / Worker (both on by default, read every round, effective immediately). The new `task_constraints` table uses `CREATE TABLE IF NOT EXISTS`, auto-created on old databases.

### Interception

#### Added

- Command interception gains model fallback approval beyond regex/string rules: when no rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, with the failure fallback action and approval timeout action configurable; the interception page splits into two tabs "Interception rules / Model config", and model judgments are marked with the `[Model]` prefix and reason.

#### Fixed

- Hardened judge output parsing to avoid a correct `DENY` judgment being mistakenly treated as approval.

### Traffic

#### Changed

- Traffic recording now stores whole exchanges in SQLite (large bodies spill to hash-deduplicated blob buckets), with a trigram full-text index on text bodies supporting arbitrary substring and Chinese-text search; deletion degrades to a single SQL transaction, dropping from hours to milliseconds without stalling recording. Added streaming download of large bodies and `traffic_search` full-text parameters; old file-tree data needs no migration and remains readable, searchable, and deletable.

### Tasks and Assets

#### Added

- Task creation gains an "asset coverage" toggle (default on): when off, coverage is neither computed nor displayed, the status graph shows only assets, `task_scope` is not auto-accumulated, and related tools are removed from the Planner / main Agent; company associations are unaffected by this toggle.
- `insert_assets` gains a `related` flag per asset (default `true`): effective only when coverage is on; `false` only enters the shared asset store and does not count toward this task's coverage (e.g. side sites or unrelated assets discovered in passing).
- Company scope and task test assets uniformly support domain, URL, IP, CIDR, ICP, and keyword text recognition; domains/IPs can create or reuse global assets, while CIDR/ICP/keywords are kept as task scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one pass, and produces a zip release package with `skills/`, config examples, and README.
- Uses the Go linker to strip debug info and produce the zip release package; UPX is now explicitly optional, avoiding segfaults when its self-extracting ELF starts on some Linux environments.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and provides `SHA256SUMS` with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks support linking multiple existing tasks, read-only inheriting in real time the direct source tasks' facts, findings, completed intents, asset scope, and blackboard context; new tasks still use an independent execution queue, working directory, and conversation history.
- Tasks support an ordered LLM config chain. When a provider's quota exhaustion is clearly identified, it automatically switches to the next config and persists the current config, exhausted state, and structured audit activity.
- Running and paused tasks support editing the LLM config chain, reordering, and manually switching the current config; auto-switch, manual switch, and full-chain exhaustion are shown on the task details page.
- Running Workers support pause, resume, and cancel. Pausing preserves blackboard data; canceling transactionally cleans up the intent and its directly produced facts, findings, and execution records after the Worker stops writing.
- Deleting a task can optionally clean up associated assets, traffic, findings, and task landing files, and gains a deletion barrier, concurrency protection, and auditable deletion statistics.
- Added an optional task concurrency cap; new tasks at the cap queue in FIFO order and auto-start when a running slot frees up.
- Added task asset pagination, company asset pagination, task category statistics, and paginated intent mock contracts.
- Added the `build.sh` single-binary build script, supporting static frontend export, resource embedding, cross-platform targets, and build version injection.

### Changed

- Task-level explicit LLM config chains coexist with the existing global LLM Pool; explicit chains keep strict quota-failover semantics, and when there is no explicit chain, Agent binding and global config rules apply.
- The main Agent input box supports auto-growing multi-line input, `Enter` to send, `Shift+Enter` for newline, and avoids accidental sends during IME composition.
- The Planner, Workers, and main Agent share the task-level LLM runtime; the task chain uses the smallest context window among candidate models as the safe compaction threshold.
- The task details page shows running, idle, and paused states by real LLM call status; source tasks' facts, findings, intents, asset references, and graph nodes are uniformly source-marked and kept read-only.
- Agent preview, task reports, and related detail views uniformly use a shared Markdown rendering component.
- LLM model config uses card and drawer interactions, with the model list independently scrollable inside the drawer.
- Task pause no longer blocks the main Agent conversation: the main Agent orchestration conversation is independent of task pause, and new messages can still be sent while paused (pause only terminates the current in-progress round).
- Worker single-run wall-clock default duration adjusted from 600 to 1200 seconds.

### Fixed

- Removed the main Agent console's optimistic echo, switching to pure server-side data rendering, fixing message disorder and bleed of other conversations' content in scenarios like pause or send failure.
- Opening a main Agent conversation scrolls to the bottom by default and fully shows the last reply: after lazy-loading expands the last reply's full content, it auto-sticks to the bottom and is no longer pushed off-screen.
- Fixed the main Agent conversation still running after pausing a task, and the orchestration Agent not synchronously stopping the main Agent when pausing the task.
- Fixed task status badge and action-button state being out of sync when a task completes or the Planner, Worker, or main Agent is actually running.
- Fixed potential late blackboard writes or residual files between Worker cancellation, task deletion, and concurrent writes.
- Fixed broken Markdown preview, long-text overflow in delete confirmations, mobile width, and misaligned buttons in some task details.
- Added named termination reasons to all Agent cancellation paths; activity details can show the canceler, terminal state, run rounds, duration, token usage, and unreturned tool calls.
- Fixed the race where the parent context could preemptively overwrite the named `shutdown` reason during backend shutdown, and the garbled text from truncating non-ASCII activity summaries by bytes.
- Fixed cancel events with partial streaming output losing the real termination reason, while preserving the content generated before cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
