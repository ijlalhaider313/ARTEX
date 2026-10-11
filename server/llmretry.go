package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side resolution of retry policy, see docs/LLM-retry-design.md. Among the five layers:
//   - Connection / empty-response / same-provider safety window are "per-endpoint" — each LLM
//     profile can override the global default (a blank field inherits the global, and if global
//     is also blank the built-in default applies);
//   - Circuit breaker / intent replay are process-level — there is only one global copy.
//
// The global policy reads a single DB settings row; call sites are all on low-frequency paths
// (building a provider, work wrap-up, saving config), so an extra cache layer is not worthwhile;
// the circuit-breaker parameter is the exception — it is read on every failure path, so
// applyRetryPolicy pushes it to the Registry for storage.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// Counts here keep the original "0=default / negative=off" semantics: SDK's
		// MaxRetries / EmptyResponseRetries are isomorphic to this, let it parse them itself.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// Circuit breaker (round-robin cooldown) defaults, consistent with llmpool built-ins —
// only overridden here when the user configures a value.
// Intent replay defaults are in engine.go: modelErrorRetries / modelErrorRetryBackoff.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob — "empty
// response retry count": the two are two approaches to the same thing. The SDK
// layer handles "not a single content block", by resending the same request as-is;
// here we handle "only thinking, neither body text nor tool calls", by appending an
// instruction to have the model continue from its existing thinking (resending as-is
// is meaningless for this kind of empty turn determined by context shape). The
// emptiness criteria differ because the SDK judges by "whether any event was yielded",
// and a thinking delta is itself an event — but when the user configures "how many
// empty-response retries", what they mean is "if the model produces no substantive
// content, try again", and the two layers sharing one count matches that mental model.
//
// Reads the global policy rather than a profile's override: a run may switch profiles
// mid-flight due to failover, and this is the total gate for the entire intent, so it
// should not change when the endpoint changes. Semantics are isomorphic to SDK's
// emptyRetries(): 0 = default defaultEmptyTurnNudges; -1 (negative) = disable empty-turn
// continuation; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
