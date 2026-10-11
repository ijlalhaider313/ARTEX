# Side-Channel Long Conversations and Context Budget

Fixed 2026-09-11. The original implementation treated the character count of the request JSON directly as tokens and inherited the main task's 32K output reservation, so HTML, JS, and tool-heavy results would prematurely reject normal side-channel questions.

## Open-Source Implementation Review

- [Grok CLI side-channel context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts snippets from recent user/assistant text, character budget ~2000, at most 400 characters per entry. Does not maintain continuous side-channel Q&A history on this path.
- [Grok CLI standalone request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): independent cancel signal; output cap of 2048 tokens when the model supports it; no tools provided.
- [Grok CLI main-session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, retains recent content, updates new content into the old summary, and handles cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full request estimation, output/buffer reservation, recent content plus rolling summary, no-tool summary request; this implementation defaults to a recent budget of 8000 and a summary output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): only attempts overflow recovery when no assistant output has started yet; the recovered call does not re-enter the same overflow recovery path.

ARTEX borrows the approach of independent output budget, recent content with rolling summary, and limited recovery. It preserves norma v0.3.6's structured messages and tool pairing, does not copy Grok's text excerpting, and does not write OpenCode's main-session compaction events into the ARTEX main transcript.

## Request Budget and Execution

- Messages follow norma's per-content-block UTF-8 byte estimation with a 4/3 margin; system prompts, tool schemas, and message envelope overhead are additionally accounted for. The estimate is not an exact model token count.
- Side-channel output defaults to at most 8192 tokens, and does not exceed the main profile's configured output cap. The service env var `ARTEX_BTW_MAX_OUTPUT_TOKENS` can set a limit of 256–32768; it does not modify the product default model or main task parameters.
- Input budget is the context window minus the output cap and safety margin; unknown windows use the platform default of 200K. The safety margin is 5% of the window, minimum 128, maximum 8192 tokens.
- Successful Q&A is loaded in batches of at most 20 pairs by increasing ordinal. At most 20 pairs of original text are retained, with a token budget of at most 1/4 of the input budget, capped at 16K.
- Excess Q&A is updated into a rolling summary. The summary carries historical source and context time; historical assistant answers are not equivalent to new tool evidence — on conflict, the latest main snapshot takes priority.
- When the main context is still too long, only old messages in the summary copy are compacted, keeping at most 8K tokens of recent content; the truncation point never splits a tool call from its result. An oversized single pair goes into the summary as a whole.
- Summary input is chunked in a UTF-8-safe manner according to the actual remaining window, with an output cap of 2048 tokens; empty summaries, truncation, tool calls, or exceeding the summary budget are not cached. A single side-channel call has at most 12 summary calls, all under the same 120-second timeout; reaching the limit fails explicitly, no infinite loop.
- If the model first returns a context-over-limit error and has not yet output text or tool calls, it retries at most once after further reduction; if the estimated size has not decreased, recovery stops immediately. Other model errors and partial streaming output do not trigger this recovery.
- All usage obtained — including summaries, failed attempts, and cancellation-time usage — accumulates into the same side-channel request. When the provider does not return usage, zero is still the only recordable value; estimates cannot be disguised as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the old Q&A summary, overridden ordinals, and the main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation phase, actual replay pair count, summary usage, and budget estimate.

The summary is saved only when the original request is still running and the cleanup version matches; clearing also clears the cache, and a late write does not restore already-cleaned data. A new snapshot does not reuse an old snapshot's summary. Summary fields are saved with v3 task archives; restoring an old v3 with missing fields backfills empty objects, and v1/v2 remain compatible.

POST accepts and returns the request first; preparation and compaction run in the background, without holding the admission lock or a database transaction. SSE/history shows the preparation, Q&A arrangement, copy compaction, and answer phases; compaction failure is saved as the failed terminal state of that side-channel request. The frontend retains the error and restores the draft of the failed question, without covering the input box with a floating notification; history polling no longer clears submission errors.

## Validation Record

- 19/20/21/50 pair replay, cross-20-pair old-conclusion retention, summary cache reuse after restart: automated pass.
- Oversized Chinese answers and code context, chunked request budget, tool pairing, snapshot invariance, new-snapshot cache invalidation: automated pass.
- Summary failure/cancellation/truncation/oversize/tool return, cleanup race, call limit, one-time-only overflow recovery and partial-stream no-retry: automated pass.
- Independent PostgreSQL pagination, restart, v1/v2/v3 archiving, summary cache and budget metadata archive restore, old v3 missing new fields, 20 parent sessions sharing four concurrent slots: pass.
- Go candidate service build, frontend TypeScript check, Biome check on modified components, and Next.js production build in an independent directory: pass.
- The built-in browser uses an independent UI fixture, validating the arrangement phase, summary scope hint, draft recovery after failure, no floating error notification, no horizontal overflow, and no console errors at 1280×720 and 390×844. The temporary fixture has been removed.
- Read-only replay of an existing Worker snapshot on this machine passes the new budget check; for example, Worker #3's 293085-character snapshot is no longer wrongly rejected by the local character count. This item has no external model calls.
- The real-conversation test with Grok using a private Worker snapshot was rejected by automated approval review this time, was not executed, and is not counted as a pass.
- 2026-09-11 00:37 — restarted the local backend per user request, retaining the original database, data directory, and login configuration. The running file matches the candidate binary SHA-256; both the backend and frontend proxy `/api/health` return normal.

Validation commands (using only the independent test database):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses an independent copy to avoid overwriting the current preview's `.next`. The candidate service is at `/private/tmp/artex-btw-budget-candidate`, has been copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.