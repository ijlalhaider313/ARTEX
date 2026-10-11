package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "User paused the task",
		"The user paused the task via the task control endpoint (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; in-flight intents are returned to the frontier (open) and will be re-claimed and re-executed from scratch when the task is resumed")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestration Agent paused the task",
		"The orchestration Agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; in-flight intents are returned to the frontier (open) and will be re-executed after resuming")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the delete barrier has cancelled the task's running Planner, Worker, and main Agent; this run's results will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task's paused state",
		"On startup, the backend restored the task's pause state from the persisted database state. This run was cancelled; normally no Agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "Planner determined the task goal is met",
		"The Planner determined the task goal is met and set the task to done, then cancelled any still-running Workers; these intents are marked as stopped, not as failures")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task timeout settlement drain exhausted",
		"The task reached timeout and waited for running Workers to gracefully settle, but the 90-second drain grace was insufficient, so a hard cancel was performed; intents are marked as exhausted, and facts and assets written during the settlement phase are retained")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent",
		"The Planner called kill_work to actively terminate this intent, usually meaning the direction has drifted or no longer has value; the intent is marked as stopped and will not be automatically re-claimed")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused a running Worker. This call was cancelled and the intent transitions to paused; all registered intents, facts, findings, and activity records are retained; it will re-execute from scratch after resuming")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this Worker intent",
		"The user deleted a running Worker. This call was cancelled; after the Worker exits the write zone, the server processes the intent per the user's chosen deletion mode — soft delete only marks it deleted and retains all outputs, hard delete cascades to remove the intent and downstream nodes supported solely by it")
	AbortWorkFinished = cause("work_finished", "Worker finished normally",
		"The Worker finished normally and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, it means a race between cancellation and the completion event")
	AbortPausedRaceGuard = cause("paused_race_guard", "New run rejected while task is paused",
		"While the task is in the paused state, the engine refuses to issue new execution contexts, to prevent a race between claim and pause from allowing Workers to start; claimed intents are returned to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this conversation turn",
		"The user clicked stop, actively aborting this main Agent or session Agent turn. Activity records already produced are retained; the next message can be sent as usual")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause also aborted the main Agent",
		"When the user paused the task, the running main Agent conversation was also synchronously cancelled. Activity records already produced are retained; resuming the task will not automatically replay this turn's message")
	AbortChatTurnFinished = cause("chat_turn_finished", "Conversation turn finished normally",
		"This conversation turn finished normally and the server is releasing this turn's context resources. This is not a run interruption; if it appears in an interruption message, it means a race between cancellation and the completion event")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running Agents are cancelled; residual running intents are reset to open and re-executed after restart")
	AbortRunHardTimeout = cause("run_hard_timeout", "Per-run hard timeout backstop triggered",
		"A single run exceeded the soft wall-clock budget plus additional grace, indicating that a model request or a tool took too long to return, preventing normal turn-boundary settlement from executing. Focus on checking the last unreturned tool call before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context reached deadline",
			"Upstream context reached deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceller did not attach a named cause",
			"Upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; please register a cause in agent/cancelcause.go and wire it into this cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
