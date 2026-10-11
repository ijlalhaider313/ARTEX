// Package notify implements the IM / email delivery channel adapter layer for
// vulnerability findings.
//
// Layering: this package is a **leaf package** that only depends on the standard
// library. It knows nothing about the database or the server. Channel config
// arrives as map[string]any (matching the notification_channels.config JSONB
// column), and the content to push arrives as a Message. Splitting it this way
// means the pieces that actually tend to be wrong — signature calculation,
// UTF-8 truncation, filter matching — can be unit-tested without PostgreSQL,
// while the host only does orchestration on the server side.
//
// Concurrency contract: Channel implementations must be **stateless**. The same
// Channel instance is reused concurrently by multiple channel configs (even
// multiple bot instances of the same channel), and all credentials are always
// passed in via the cfg parameter. Caching a webhook URL or anything similar
// inside the implementation's own fields is not allowed.
package notify

// Channel type identifiers. The set of values is also the legal set for
// notification_channels.kind, validated by an allowlist on the server side
// (same rationale as findings.status: no DB CHECK, so adding channels later
// is easy).
const (
	KindDingTalk = "dingtalk" // DingTalk custom robot
	KindFeishu   = "feishu"   // Feishu (incl. Lark) custom robot
	KindWeCom    = "wecom"    // WeCom (Enterprise WeChat) group robot
	KindWebhook  = "webhook"  // Generic webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, matching notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value when config has an empty kind.
const InitKind = KindDingTalk

// severityRank maps vulnerability severity to a comparable ordinal. Unknown
// severities return 0, so any min_severity setting excludes unknown severities
// — when in doubt, don't push, to avoid flooding with false positives.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the ordinal for a severity; unknown severities return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the emoji-prefixed severity label used in message
// titles and card colors. Unknown severities are echoed as-is, never invented.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel translates a handling status into a display label for
// status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In Progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False Positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk Accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min means
// no threshold is set and everything passes. Note that an unknown severity has
// ordinal 0 and is rejected by any non-empty min (see severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}