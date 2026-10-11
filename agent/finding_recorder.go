package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**Finding traffic evidence (optional)**: When calling report_finding to report a vulnerability, if you have already viewed and confirmed HTTP requests/responses that support the finding, you can bind real traffic IDs via traffic_refs in reproduction order; domain and time are only candidate filters and do not imply association. For TCP and other non-HTTP vulnerabilities, or when no capture or exact match exists, omit traffic_refs or pass [] and keep command output, logs, and other verifiable evidence in the evidence field; it is recommended to explain why no binding was made. Do not guess IDs, and do not re-probe solely to fill in missing traffic."

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
