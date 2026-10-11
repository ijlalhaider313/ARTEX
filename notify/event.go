package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column.
// The writer is the db-layer vulnerability persistence transaction; the readers
// are the server-layer delivery engine and filter matching. The definition
// lives in this package because it is the payload of the "notification domain":
// db only serializes it and does not understand the field semantics.
//
// Why redundantly store vulnerability fields instead of re-querying at render
// time: vulnerabilities get renamed, re-severitized, and re-statused after the
// fact, and the pushed content should reflect the conclusion **at the time of
// the event** — re-querying would yield the dangerously misleading "changed to
// low afterwards" result. It also means fan-out and rendering need not JOIN the
// findings/tasks/assets tables.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is a single vulnerability to be pushed, for channel rendering.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets is the parsed asset display names (e.g. domain/IP). Filled in by
	// the server layer — this package does not touch the database and cannot
	// resolve names.
	Assets []string
	// DetailURL is the vulnerability detail link; empty means public_base_url
	// is not configured, and rendering omits it.
	DetailURL string
	// Status-change event only; when both are non-empty it renders as
	// "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the display title for the item: prefer the human-named name,
// fall back to the vulnerability class vulnclass, and when both are empty use a
// placeholder — never emit an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(Unnamed vulnerability)"
}

// Message is the complete content of a single channel send.
type Message struct {
	// Length 1 for a single push; a full batch for a digest push.
	// An empty slice is invalid; the caller must guarantee at least one item.
	Items []Item
	// When Batch=true, render as a digest message (different title, include the
	// time window and item count).
	Batch bool
	// WindowMinutes is the digest period (minutes), used in the copy "Last N
	// minutes" only when Batch=true. Deliberately passed in explicitly from
	// config rather than computed via time.Since at render time: rendering
	// stays deterministic, which makes it testable.
	WindowMinutes int
	// HomeURL is the platform dashboard address (the global public_base_url);
	// when empty, no dashboard link is included.
	HomeURL string
}