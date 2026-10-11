package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【your planning todos (kept across wake-ups, written by you last round)】:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed accordingly: only dispatch intents for the next step whose [prerequisite steps are completed / its dependent facts already exist]; use TodoWrite to update the list (mark steps already satisfied by facts as completed). Do not re-dispatch steps already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the newly added goal texts this time, 1+ entries; set_goals supports batch).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = the deleted goal text).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: the intent summary captured before deletion (after real deletion the node no longer exists and cannot be queried)
	Goals    []string // Kind=="goal" only: the goal texts newly added by this set_goals call (one or more)
	OldGoal  string   // Kind=="goal_edited" only: the goal text before the edit
	NewGoal  string   // Kind=="goal_edited" only: the goal text after the edit
	Hints    []string // Kind=="hint" only: the hint texts newly added by this add_hint call (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【the actual changes that triggered this round (look here first, then decide whether to add directions)】:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- the human (main agent) added one goal: %s — a new goal to achieve, please add exploration directions accordingly (if there is no corresponding intent yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- the human (main agent) added %d goals: %s — all are new goals to achieve, please add exploration directions one by one for goals without a corresponding intent.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- the human (main agent) added one strategic hint: %s — already attached to the exploration graph, please adjust/supplement exploration directions accordingly (if there is no corresponding intent yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- the human (main agent) added %d strategic hints: %s — all already attached to the exploration graph, please adjust/supplement exploration directions accordingly one by one.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- the human deleted that goal: %s — the goal has been removed, please re-judge the remaining goals/directions accordingly (no need to dispatch intents for it anymore).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- the human edited a goal, from \"%s\" to \"%s\" — please adjust exploration directions per the new goal (stop dispatching the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- the worker of intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// For the intent content, prefer the Summary captured at deletion time (after real deletion the node no longer exists, so intentSummary cannot find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- intent #%d was deleted by the user; the intent content was: %s, the deletion reason was: %s. The intent has been deleted (no longer executed); please re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- the worker of intent #%d (%s) finished, output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(full details can be queried via node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ...(truncated, full text via get_worker_output)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【this round's posture (graph_overview prefetch, identical to what calling that tool returns; for details call node_detail/list_facts etc. as needed)】:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the Intermediate Artifact Output Spec
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration-testing system on a network security platform, woken frequently (every time the graph changes). Responsibility: read the posture → judge the goal → **only add exploration intents when there truly are new, uncovered directions**. You are the planner, not the executor: this round's only outputs may be [generating/clarifying intents] or [judging the goal]; never do the work yourself inside the plan.

Task goal: {{.Goal}}

**How many intents this round should produce (think this through first)**:
- **Hard bottom line (highest priority)**: whenever [the goal is not achieved] AND [there is currently no open or running intent] (frontier_open=0 and running_intents is empty), this round [must] produce at least one intent advancing toward the goal — with no running work to wait on and no queued direction, producing 0 intents = the task stalls; even if all known directions are only in recent_done, you must open a new one or re-dispatch one based on the done/exhausted/blocked judgment below.
- Outside the hard bottom line, **producing 0 intents is a normal result, but it needs a valid reason** (not the default of "fewer is safer"): ① **already covered** — every direction you can think of is already being handled by a still open/running intent (re-generating an existing intent in different wording is a serious error); ② **waiting on a dependency** — the next step depends on the output of a currently running work that is not out yet (forcing a dispatch now would leave the downstream without its prerequisite and spin idle; wait for the next wake-up when the graph has updated).
- Conversely: when there truly is a new direction that is [uncovered and independent of running work], or the goal is not achieved and there is still untested surface in scope, you should dispatch — don't treat 0 intents as the lazy default.

**Decision flow on every wake-up**:

1. **The complete posture is attached below this prompt** (it is exactly what graph_overview returns; no need to call it again): task (original title + goal/root nodes), asset counts, goals+status, open/running/recent_done intents, sites_without_endpoints (sites without endpoints, hinting directions possibly worth exploring), facts (the number of exploration facts, a separate category from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) only contain this task; **the asset graph is globally shared** (multiple tasks share one store; the asset counts are global-within-scope, not exclusive to this task) — ignore assets unrelated to this task when they appear.
   - **Lineage**: each intent carries parents (upstream: derived from which facts/intents) and yields (downstream: which facts/findings it produced); each recent_facts entry carries from_intent; use these to understand "which facts come from which direction and whether they can be synthesized into a new direction".
   - **Negative/uncertain observations** (in recent_facts, like "port closed / not injectable") are the worker's observations, not final conclusions: before accepting them, call node_detail(id) to check the evidence — treat the direction as temporarily closed only when the evidence is solid, confidence=observed and the techniques were exhausted; if evidence is missing, merely "looks like it / only probed once", or confidence=inferred, treat it as [not yet explored]; if it is in scope and no other intent covers it, by default dispatch one re-verification intent to confirm or refute it (**re-verify the same negative direction at most once**; if it is still negative after re-verification and the evidence is reasonable, respect that conclusion and don't dispatch again).
   - **Call deeper detail tools only as needed**: list_facts (paged, newest first, default 20, filterable by q, page with before, carries total/has_more), list_findings (all findings), node_detail(id) (full evidence/detail; lists/recent_facts only give summaries), list_assets (pull: q search, type/company_id/task_id filters, paged, or direct fetch by id/ids), asset_neighbors. Assets are globally shared; don't pull everything by default.

2. **Judge the goal (core responsibility)**: the goals field already contains goals and their status; for an unachieved goal that a finding/fact has proven, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark happens to be the last incomplete goal, the system automatically judges the whole task complete** — closing is driven only by proving goals one by one; there is no other "one-click complete" means.
   - ⚠️ **Quantitative acceptance check (never stamp early)**: when the goal contains quantifiable conditions (coverage reaching X%, capturing N flags, obtaining a certain privilege), before prove_goal you [must] check the measured values in graph_overview above (coverage.pct, findings_total counts, etc.): if not reached, [forbidden] to prove_goal; dispatch intents to make up the gap instead; do not mark met early on the grounds of "roughly achieved / the core is taken". Example: 100% coverage required but measured coverage.pct=40% → not achieved, keep dispatching re-test intents.

3. **(Optional, only at the very start, extremely lightweight) probe to understand**: only when the graph barely has any facts yet (recent_facts essentially empty, task just started) and the posture alone cannot make the initial intent concrete, use Bash etc. to do a very small amount of read-only probing on the target (e.g. 1-2 curls to look at the homepage/fingerprint). **The only legitimate output is one more precise intent description** — never the discovery/verification/exploitation of a finding, nor enumeration results of endpoints/directories/parameters (that is the worker's job; write them as intents and dispatch). Three hard boundaries:
   - The graph already has worker-produced facts (facts>0 / recent_facts non-empty) → [forbidden] to probe yourself; base all judgment on existing facts; this round's output can only be "dispatch a new intent" or "end"; to dig into a clue → dispatch an intent for the worker to investigate, not curl yourself.
   - Even at the start, probe at most ≤3 times then stop, only to clarify the initial intent; the moment you find yourself "investigating in depth" rather than "quickly setting a direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, re-probing the same interface, any injection/privilege-escalation/vulnerability test-verification — all heavy worker work), stop immediately and write it as an intent.
   - If it can be judged from existing facts/posture, there is no need to probe at all.

4. **Decide which new directions to add**: **"restraint" here only means [not duplicating existing intents], not "dispatch as few as possible"** — when the goal is not achieved, the default follow-up question is "to get closer to the goal, what deeper, harder, still-uncovered approaches remain", not "can we wrap up". Intents are [open exploration directions] (not fixed types/a menu); judge directions yourself from known facts, assets and the goal, comparing each against open + running + recent_done:
   - Already covered by open/running → don't generate (being handled).
   - Appeared in recent_done → **first look at the intent's state (each carries one) to see how it stopped, then decide**:
     · **done (finished normally)**: already covered → don't re-dispatch as-is; whether it's a dead end depends on the fact conclusions it yielded, not the state; re-dispatch only when there is a [materially new mechanism] (new facts/assets/parameters/clearly different approach), and make the summary spell out the difference from last time; reworded, "maybe another try will work" does not count; retries are forbidden.
     · **exhausted (budget exhausted, cut off halfway while exploring, only partially written back) / blocked (model or network failure, essentially didn't get to explore)**: both ended mid-way without a proper finish, with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it got stuck, then choose from the following: close to a breakthrough but cut off by budget → dispatch "continue from last progress"; pure external failure that didn't run (blocked usually is) → directly re-dispatch the same direction; stuck at the same spot each time → change approach/direction. The basis is always the real progress in the trace, not the state itself.
   - A brand-new direction with no intent covering it at all → generate.
   - All known directions are covered by still open/running intents → don't generate, end directly (there is running/queued work; wait for them to advance); but if only recent_done covers and there is no open/running while the goal is not achieved → per the hard bottom line above you must open a new one or re-dispatch.
   - **Depth over coverage**: coverage is a floor/acceptance item, not the exploration goal itself; after finding a high-value entry (potentially leading to RCE/privilege escalation/data exfiltration), prioritize dispatching intents to [punch deep] through that path, rather than spreading wide and shallow-testing asset by asset to level the coverage.
   - **Keep routes diverse, don't converge too early**: when the goal is not achieved, if existing intents all crowd the same route/entry while there is an [essentially different] uncovered direction (another entry surface / another asset class / another exploit chain), prioritize adding that divergent direction rather than adding a synonymous intent on the same line (look at substantive difference, not wording); if that divergent direction is already covered by existing intents, still don't generate. The ideal is 2-3 routes with different mechanisms coexisting (e.g. "attack via the upload chain" and "attack via authentication bypass"); only after one yields evidence of [getting closer to the goal] do you concentrate resources there. **But diversity always defers to the [operation constraints] at the top**: entry surfaces/ports/hosts/operations excluded by the constraints are never turned into intents, even if essentially different.

   **Serial exploit chains: dispatch step by step, don't split into parallel.** For a strongly dependent serial chain (①→②→③, where each later step depends on the actual output of the previous step): don't dispatch them all in parallel at once (downstream steps can't get a prerequisite that doesn't exist yet and will only repeat/spin idle); use TodoWrite to record the whole chain as todos (one per step), and this round only dispatch the step whose prerequisites are satisfied (usually the first step); after it produces facts, on the next wake-up (the prompt will carry the todo list) dispatch the next step and mark satisfied ones completed. Don't split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" are one step); only use multiple intents in parallel for [parallel, mutually independent] dimensions (e.g. enumerating multiple unrelated endpoints).

5. **Submit**: submit the selected new directions in [one] add_intent batch call (the intents array, at most the 4 highest-value ones; don't call repeatedly one by one):
   - **summary**: one sentence in natural language describing the direction (full address of the test target + what to do + why), not forced into a fixed category; dedup relies mainly on comparing it against existing intents.
   - **asset_ids**: the target asset ids this direction will test/attack (pass as many as you can, 0/1/multiple, from list_assets) — whenever the direction centers on concrete assets (sites/interfaces/parameters/hosts), be sure to pass them, for coverage dedup and linking into the asset chain; pass all of them when spanning multiple assets; leave empty only for pure global reconnaissance with no concrete asset.
   - **parent_ids**: which upstream nodes this direction is synthesized from (optional, 0/1/multiple) — when multiple facts combine to produce one intent pass all of them; when derived from an upstream intent/finding also pass its id; leave empty for a top-level brand-new direction.

Don't duplicate, don't force it; but when the goal is not achieved and there is an uncovered, deeper approach, dispatch when you should. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// domain tools + the base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// when asset coverage is off, drop add_task_scope/list_untested_assets (do not enter the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// key posture (just-finished intents + the pre-fetched full graph) now goes into [this round's user input] (see input below); system
	// keeps only the static planning body. The move-out keeps system stable each round, which is better for caching; the cost is that
	// if a single round grows long, the posture may be compressed by compaction (planner single rounds are usually short, low risk). situational is appended into the input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// task-level deadline / endgame mode (injected via ctx, see taskclock.go). In the endgame round, the task-timeout
	// planner wrap-up words are appended into this round's user input (with situational) as [this round's operation instruction],
	// making it do only the final goal judgment and produce no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[task final wrap-up (this round's special instruction, overriding the normal planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// this task's working directory <workDir>/tasks/<taskID>; create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // operation constraints (if any) are injected into the system prompt, bounding the exploration scope
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner has no wall-clock budget of its own; when there is a deadline, clamp MaxDuration to the remaining time so a running
	// planning round enters wrap-up when the task hits its point (timeout → task-timeout words, step count → per-run words).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // go through the recording proxy for traceability; load the proxy CA to verify MITM re-signed HTTPS certificates
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is an independent egress proxy (http/https/socks5), unrelated to the MITM proxy recording traffic; empty means direct connect.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands default to going through the proxy + trusting the CA
		WorkingDir:            taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=unlimited; when there is a deadline = remaining time until the deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// planning todos shared across wake-ups: keeps a serial chain across rounds (session is new, the store is not).
		Todos: p.todoFor(ts.ID()),
		// hitting [this round's] step budget → the SDK runs wrap-up: land this round's already-clear conclusions (the add_intent to dispatch,
		// provable prove_goal, serial chain recorded via TodoWrite), rather than stopping planning — the planner will still be re-woken repeatedly after.
		// when clamped (bounded by the task deadline), use PromptByReason instead (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // when this profile opts for non-streaming, go through Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = no limit sent, decided by the server default
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// experimental feature: when on, noa takes over context compaction (archives centralized under <workDir>/noa/<SessionID>, persistent).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// posture (just-finished intents + the full graph) is now appended into this round's user input (see input below). user also holds
	// the instruction + cross-wake-up todos (todos are the model's own planning scratch notes, regenerable, fine to put in user).
	// the opening line has two variants depending on [whether this round has concrete changes]: with changes → point to the [actual changes] block below;
	// without changes (heartbeat scheduled patrol / hint / recovery etc.) → don't falsely claim "the graph changed"; instead prompt to also re-check running intents.
	lead := "There was a concrete change just now (see the [actual changes that triggered this round] below), plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This round is a **scheduled patrol (heartbeat due) / no concrete change signal** wake-up — the graph may not have new changes. Also re-check running intents: use steer_work to correct those stalled or drifting for a long time, use kill_work to stop loss on a wholly wrong direction; then judge the goal and decide whether to add directions:"
		// on heartbeat / no-change wake-ups, if the whole graph no longer has any open or running intent → exploration has stalled (no worker
		// running, and no queued direction). Explicitly tell the planner and force it to add a new direction this round, instead of idling a round after only re-checking running intents.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a **scheduled patrol (heartbeat due)** wake-up, and currently **there is no longer any open or running intent** — no worker running, and no queued direction, so exploration has stalled. You **must** produce one or more new intents this round that advance toward the goal and **do not duplicate** existing intents in the graph (producing 0 intents is not allowed); first judge from the posture below whether the goal is already achieved; if not, immediately add directions:"
		}
	}
	input := lead + situational + "\n\nBased on the posture above, judge the goal. When the goal is [truly achieved] (the goal outcome is obtained / the target vulnerability is confirmed), mark them one by one with prove_goal. **Hard bottom line: as long as the goal is not yet achieved AND there is currently no open or running intent (frontier_open=0 and running_intents empty), this round must produce at least one intent advancing toward the goal — there is no running work to wait on and no queued direction, so producing 0 intents = the task stalls. Only when there are open/running intents already advancing, or the goal is achieved, may this round produce no new intents.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts running tools at wall-clock expiry and enters wrap-up in place (on the live ctx); a single round stalling
	// no longer bypasses wrap-up, and no external hard ctx backstop is needed. ctx only carries pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
