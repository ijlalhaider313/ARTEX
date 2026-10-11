package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns "built-in tools" from pure code into an enumerable, DB-overridable catalog:
//   - BuiltinToolSeeds(): expands the three executing agents' built-in tool sets into seed records (key +
//     description + parameter schema + default-bound agents), for the server to idempotently seed into the tools table on boot.
//   - ToolResolve hook: at runtime, post-processes the assembled tools against the DB tools rows — "per-agent
//     filtering + description/schema override + parameter default injection". The key/handler stay in code; the DB
//     only changes "prose and defaults".
// The handler (Call behavior) always comes from code — the DB cannot change it, only the description and parameter
// defaults the model sees.

// ToolSeed is a seedable snapshot of a built-in tool: Key is CoreTool.Name() (hard-bound to the handler,
// read-only in the UI), Desc/Schema come from the tool definition in code, Agents are which agents the code
// gives it to by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // top-level description (overridable in the UI)
	Schema map[string]any // parameter JSON-Schema (structure read-only, description/default editable in the UI)
	Agents []string       // default-bound agent keys (worker/planner/mainagent)
}

// builtinToolsByAgent constructs each executing agent's domain tool set using a "read-only empty shell" ToolSet
// (nil stores). The tool constructors only stuff closures into the Spec and never dereference stores during
// construction, so nil is safe — these tools are only used here to read Name()/Description()/InputSchema(),
// never Call'd.
//
// Deliberately [excludes] the SDK generic tools actool.DefaultTools() (Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash): every agent always owns them, there is no "bound to whom" decision, and most of their descriptions
// live in Prompt() (this table only covers Description(), which would be a half-coverage trap). Not seeding → no DB
// row → ToolResolve passes them through untouched, behaving as before. Only artex's own domain tools enter the table
// to be managed.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (the goal decomposer) is by default bound to set_goals + set_constraints: via them it writes the
		// decomposed goals and extracted operation constraints into the store. Shares the same managed tool as mainagent;
		// the web UI can edit description/schema and check per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto is by default bound to finding-report + asset-management tools; other domain tools can be checked in the UI as needed.
		// New stores are seeded from this; old stores are migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (the standalone pentest agent) is by default bound to: query assets / insert assets / report findings /
		// query findings / query companies.
		// New stores are seeded from this; old stores are migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools still enter the catalog (visible in the web UI, manually checkable per agent), but
// are by default [bound to no agent] — ToolResolve drops a tool with an empty binding for every agent; explicit opt-in
// is required. They remain in some agent's base tool set (e.g. goal_met in PlannerTools) so that: one, the seed can
// construct it to get its desc/schema; two, after a user manually binds it back, the runtime base has it and ToolResolve
// keeps it.
//
// goal_met: bypasses proving goals one by one and directly announces [the whole task is complete] globally — high
// weight with misjudgment risk, and it duplicates "prove_goal marks the last goal → auto-settle", so it is by default
// given to no agent and must be manually bound when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds deduplicates and merges each agent's built-in tool set into a seed list: same-named tools (e.g. list_assets
// owned by multiple agents) merge into one entry, with Agents taking the union; tools in defaultUnbound are forced to an empty binding.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // in the catalog, manually bindable, but not given to any agent by default (stored as [] not null, consistent with other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// parameter defaults get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only defaults are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
