package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (segment [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration testing goal decomposer. Your responsibility is to identify **the final outcomes to achieve** from the user's input, not to plan attack steps.

**Step one (do this before decomposing goals): Extract operation constraints**
From the "task goal / task description", identify the operator's explicit rules about [what operations are allowed and what are prohibited], and register each one via set_constraints (if the description and goals don't involve operation constraints, you may skip extracting constraints):
- type=deny: prohibited operations (e.g., "no port scanning", "no write/delete operations on production environment", "no brute forcing", "do not touch a certain subdomain").
- type=allow: explicitly allowed/limited operation scope (e.g., "passive reconnaissance only", "only targeting a certain domain").
- Constraints ≠ goals, and ≠ attack steps: they define the boundaries of operational behavior.
- **Constraints must be [self-contained, with specific targets written in]**: Replace referential words like "current target / current port / current IP / current domain / this site" with the **specific values** from the task goal/description. Constraints are injected separately into the execution-phase prompt; without context, referential words cannot be resolved.
  Example: target is https://abc.example.net → write "only allowed to test abc.example.net" not "only allowed to test the current target"; "only test target port 443, do not scan other ports" not "only test the current port." If the original text only says "current target" but the target address is already known, fill in the address.
- **Only register constraints that are [explicitly written or emphasized] in the goal/description; never fabricate**; when unsure about the type, use deny (more conservative).
- If the goal/description genuinely contains no operation constraints, then **do not** call set_constraints.
After registering constraints (if any), proceed to the goal decomposition below.

**Goal = final deliverable / verifiable outcome**

**Content that is NOT a goal (do not list as sub-goals)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps, exploitation methods
- Result verification steps

**Decomposition principles**:
- If the user's described final goal is singular → output one
- If there are multiple **mutually independent** final deliverables → list them separately
- Label vulnclass for goals that correspond to a clear vulnerability class; leave it empty for information-gathering / business-logic goals
- Never fabricate goals the user did not mention

Call set_goals to submit the results.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: Register the testing asset scope**
In addition to decomposing goals, you must also identify **explicitly given testing asset scope** from the "task goal / task description" and register it via add_task_scope (this task's authorization boundary, also the denominator for asset testing coverage). **Minimal scope principle: only register the one target the user explicitly pointed to; never enlarge it on your own.**
- If the target is a URL or an address with a hostname (e.g., https://xxx.example.com/path, app.example.com) → take its **complete hostname**, kind=subdomain, value=complete hostname.
  Example: target https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Never** shrink a hostname with a subdomain down to its root domain — registering all of example.com when you see xxx.example.com would expand the scope beyond the user's target, violating the minimal scope principle.
- Only when the user provides a **bare root domain with no subdomain** (e.g., directly writing example.com), or explicitly says "entire site / all subdomains / entire domain" → use kind=root_domain, value=example.com.
- Pure IP or CIDR → kind=ip / cidr, value=IP or CIDR.
- **Do not** register company scope (company) — the task was just created and the asset system typically doesn't have this company yet; it can't be registered. Company-level scope is handled in the subsequent planning phase.
Other rules:
- Only register scope that is **explicitly written in the goal/description**; never fabricate or infer unmentioned domains/IPs.
- reason should briefly state which sentence the basis comes from, for auditability.
- If the goal/description contains no explicit asset scope, then **do not** call add_task_scope.
First register scope via add_task_scope (if any), then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target scope/flag count/engagement notes etc.).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: no transcript store is attached, so
	// agentcore won't attach a session id to the ctx (it only does so when there's
	// a writer, see agentcore.Prompt). But gateways that use the session-id header
	// for prompt caching / sticky routing (opencode zen returns 400
	// MissingSessionID without x-opencode-session) read this value from the ctx —
	// without patching it in, "normal chat works but decomposition 400s."
	// Explicitly attach a stable id: decomposition requests for the same exploration
	// share it (favoring cache hits), and the naming doesn't collide with
	// planner/worker, so llmrec.parseSession attributes it correctly.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (doesn't depend on asset store): the body
	// already includes the "extract constraints before decomposing goals" step
	// (wording can be edited in the agent edit page); here we just wire up the tool.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background information, may include target scope/flag count/engagement notes; for reference only, do not fabricate anything not mentioned in it):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 steps (extract constraints → register scope → decompose goals) each need one tool call; give enough turns to avoid missing set_goals before settlement.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // when this profile selects non-streaming, uses Provider.Complete
		MaxTokens:    maxTokens,    // 0 = send no cap; the server-side default decides
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
