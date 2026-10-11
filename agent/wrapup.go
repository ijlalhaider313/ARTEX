package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// wrap-up / settlement prompt: when an agent is terminated due to [step budget exhausted (MaxTurns)] or
// [timeout (run_seconds/MaxDuration)], the SDK's settlement stage injects this prompt,
// making the agent first persist what it already recognized but had not written back, then output a one-sentence summary, to avoid a half-finished run.
//
// each agent's wrap-up prompt can be overridden in the admin as needed (stored in agents.wrapup_prompt); leave it empty to use the
// built-in default here. Only the [prompt body] is editable; which tools are disabled and how many rounds the wrap-up itself gets are fixed in code.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// built-in default wrap-up prompts, indexed by agent key. worker reuses the historically hardcoded settleWrapUpPrompt
// (defined in worker.go); planner/mainagent each have their own; misses (custom agents) fall back to the generic one.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: the built-in default [own] round budget for each agent's wrap-up stage (overridable by admin >0).
// all are given 10 rounds to guarantee enough steps to persist in the wrap-up stage. Misses use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planning steps for this round are about to run out — note that only [this round] is ending; the system will still wake you again as the posture changes to continue planning, not terminate the task, so you need not wrap up the whole plan here. Please land the conclusions already clear this round, don't let this round run in vain, but also [do not force-fit intents just to wrap up] (0 intents this round is still a completely normal result): (1) if you have judged an exploration direction that [should be dispatched right now], batch-submit it with one add_intent call (don't hold back what you've decided); (2) for goals proven achieved by a finding/fact, call prove_goal to mark them met (don't miss any); (3) if you have identified a serial exploit chain needing step-by-step execution, record it with TodoWrite so the next wake-up can continue dispatching. When done, end this round directly; no summary text is needed."

const mainAgentWrapUpDefault = "Your steps are about to run out and this interaction is about to end. Do not start any new exploration/operation. Please **summarize in one standalone plain-text sentence** for the user the current progress, key conclusions, and the suggested next step."

const genericWrapUpDefault = "You are about to be terminated because your budget is exhausted. First write back results that are done but not yet persisted, then **summarize in one standalone plain-text sentence** what you did and which key conclusions you reached (this sentence will be shown as the result of this run)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- task-level timeout wrap-up words (see docs/task-level-timeout-and-wrapup-design.md) ----------
//
// This is a [separate set] from the per-run wrap-up words: per-run means "this run's budget is used up"; task timeout means
// "the whole task has hit its point and is about to end". The semantics are often opposite (especially for planner: per-run says
// "don't stop, keep planning", task timeout says "stop planning at the point and make the final judgment"). Configured only for worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB overrides for the task-timeout wrap-up words and rounds
// (wired to agents.task_timeout_wrapup_prompt / _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not this run's budget, but the whole exploration hitting its point). This is the last chance: (1) persist [all] of what you have recognized but not yet written back — new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed findings via report_finding; (2) do not start any new command/probe; (3) **finally summarize in one standalone plain-text sentence** your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not this round, but the whole task terminating). Based on [all] current facts and findings, make the final goal judgment: for goals proven achieved by evidence, call prove_goal to mark them met (don't miss any). **Do not generate any new intents** (intents dispatched now will not be executed anyway). Finish right after judging; no summary text is needed."

// TaskTimeoutWrapupDefault returns an agent's built-in default task-timeout wrap-up words (for admin placeholder / restoring default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // unconfigured (mainagent/chat) returns empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. Empty string means the agent has no task-timeout words
// (non worker/planner), in which case the caller should fall back to the per-run words.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // by default reuse the per-run rounds
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is bounded by the task deadline: wrap-up due to Timeout = task hit its point → task-timeout words;
//     wrap-up due to MaxTurns = steps exhausted first within the clamp window, the task still has minutes left → fall back to per-run words.
//   - clamped=false → the task is still early: both reasons use the per-run words (i.e. degrade to wrapupSettlement).
//
// the PromptByReason handed to the harness picks at wrap-up time by the [actual] reason on the spot, avoiding build-time mismatches.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // task hit its point
				harness.ReasonMaxTurns: perRun, // steps exhausted first, task still has time left
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
