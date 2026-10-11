# `/btw` Validation Record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Uses an independent PostgreSQL test database and data directory; real model credentials are injected only into the independent test environment, not written into code or this record, and the product default model is not modified. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

Actual model conversations, returned objects, engineering assertions, and Qwen's original review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering Checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, tool parameter deep copy | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests do not overwrite, complete reply and terminal state publish, half-reply excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-pair replay, budget trimming and over-limit error | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side parallel, bidirectional cancel isolation | Pass | Blocking provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, usage on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript / activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subcases |
| Persistence, pagination, idempotency, restart retains partial answers | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs late-write race, parent resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archive and restore, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| Three parent interfaces, auth, resource ownership, Worker logical deletion | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Busy main session can side-channel, independent SSE reconnect / disconnect, cancel, clear | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 per parent session / 4 global concurrency | Pass | Two `TestSideHTTP…` cases |
| Pre-submit snapshot persisted, restart continue-question, old session cannot forge snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Cached config deleted or model changed → reject continuing | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel and wait for final answer and usage to persist before archiving | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Streaming consumer early cancel records usage once with side-channel attribution | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| Restored Worker / deadline runtime context continues publishing new snapshots after restart | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check on related packages | Pass | Commands below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| New frontend modules Biome | Pass | `biome check`, 3 new modules |

After configuring `ARTEX_PG_DSN` against a separate disposable database, the automated checks can be reproduced (do not point at the production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: the `server` package has two pre-existing tests that fail during temp-directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting source from the unmodified baseline above and rerunning the `server` package in the same isolated environment, these two cleanup failures also reproduce. The baseline run additionally produced a target-node-count assertion failure in `TestCoreTaskLifecyclePG`; the final modified `server` regression did not have that assertion failure. Other packages pass, and this round's side-channel-related cases and race checks pass. Baseline issues are not marked as accepted-pass for this round, and no existing assertions were modified to hide issues.

Next.js build outputs the pre-existing multi-lockfile / workspace-root inference warnings; the build completes and all pages generate successfully.

## Browser Checks

Uses Codex In-app Browser, connecting to an independent local Go service and Next.js dev server. Desktop and 390×844 narrow-screen complete the following manually-automated operations, checking screenshots and browser logs:

- Enter `/btw` during regular chat; main content and side-channel display simultaneously; desktop sidebar works.
- Consecutive follow-up questions; after side-channel stop, generated partial content is retained; main flow continues.
- Closing the panel keeps the request running; reopening restores the completed answer; refreshing the page and entering empty `/btw` restores history.
- Narrow-screen Drawer input, buttons, history, and close all work, no horizontal overflow.
- Clear uses a confirmation dialog; after clearing, history disappears, main transcript and snapshot are retained.
- Task MainAgent and two Workers each get questions and are switched; Agent labels and history do not cross-contaminate.
- A blocking local model fixture keeps the Worker running; submitting `/btw` from the Worker's main input, after stopping the side-channel the Worker still shows live running and its own pause button, and the side-channel saves the partial answer.
- Browser error / warning logs are empty.

The controllable fixture is used to precisely verify concurrency timing, not relying on real model output speed. During debugging, two Worker runtime checks did not form a valid concurrency window (task already ended / answer ended early); after fixing the fixture and redoing, they passed; these initial operations are not counted as valid passes.

## Real Model Conversations

Preferred probe `grok-4.6`, OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. Probe returned HTTP 200, model name `grok-4.6` and `READY`, in 2.82 seconds. The preferred option was available, so the Tingly `glm` or Zhipu `glm-5.3` backup chain was not enabled; these two backup services were not verified this time.

| Scenario | Actual Result |
| --- | --- |
| Ask about assets, target, marker during main session | Returned `redhaze.top`, homepage read and target summary, `BTW-REAL-0910`; side-channel completed, 16.97 seconds |
| Ask about tool evidence after main session completes homepage read | Correctly cited WebFetch 200, curl redirect 301 → 302 → 200, page title; 7.24 seconds |
| Side-channel requests Bash to create a test file | Refused execution, target file not created; 7.74 seconds |
| Completed side-channel does not change main context | Main transcript SHA-256 and main activity record remain consistent; side-channel tool execution count is 0 |
| Stop / restart Go service then continue questioning | Retained previous 3 side-channel history entries, answered asset, marker, and title directly from persisted snapshot, did not rerun main Agent |
| New session using Grok non-streaming config | Correctly answered asset and `ATOMIC-0910`; returned and saved usage: input 11734, output 138, cache_read 11520 |

The asset case's main session uses WebFetch and Bash/curl to read the public homepage; the landing page is `https://id.redhaze.top/home`, titled "RedHaze Group — Global Comprehensive Group Portal". Bash caches the response in a local test file; no writes are sent to the remote. This fact is verified separately from "side-channel did not execute tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming response did not return usage. A separate direct send with `stream_options.include_usage=true` was verified: HTTP 200, 12 data frames, 0 usage frames. Therefore the 0 in streaming tests means the endpoint did not provide usage, not that there was no billing. Non-streaming usage and the fixture's failure / cancellation usage are both correctly saved.

## Qwen Review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. Provided the first three real side-channel conversations, main-session tool evidence, and engineering assertions; returned `verdict: accept`, `concerns: []`, considering the answers consistent with the asset, marker, and page-read evidence, and the side-channel tool refusal compliant with constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review scope does not include the subsequently added service-restart and non-streaming tests. Qwen's summary of "no writes" was too broad: the main session curl did create a local response temp file, as explicitly recorded above. Concurrency, zero-tool-execution, and transcript isolation are judged by engineering assertions; the model review only assists in evaluating answer quality.