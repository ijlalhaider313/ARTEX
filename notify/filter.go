package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column:
// the filter conditions for a channel instance. All fields are optional; the
// default is "no filtering" — which is exactly the fallback semantics for
// malformed config, see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical);
	// empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs: an empty array means no restriction; non-empty
	// requires the event to intersect it.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude: empty means accept all; non-empty requires vulnclass to
	// match at least one keyword. VulnClassExclude: matching any keyword
	// excludes the event (exclude takes precedence over include). Matching is
	// case-insensitive substring — safer than regex: a misconfigured regex will
	// not silently disable the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives vulnerability
	// status-change events (only meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses channel filter config.
//
// **Never returns an error.** This is a deliberate design choice: when filter
// config is malformed, it always degrades to a zero-value Filter (= no
// filtering = match everything), because for a vulnerability notification
// system, **one extra push is far better than silently dropping a high-severity
// one**. Letting a parse failure become "don't push" would give the user a
// channel that looks configured but actually pushes nothing — the worst
// failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure f stays zero-value, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a legal severity threshold (empty
// string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the fields of the filter config whose values are
// **constrained**, for use when saving a channel.
//
// Why it must be caught at write time: Match's verdict on an unknown threshold
// is `rank >= 0`, always true — meaning a one-character typo in min_severity
// ("hgih") makes the filter **silently fail** into "push everything". This is
// consistent with this package's "rather over-push than miss" tradeoff (it
// does not miss), but the consequence is that the user thinks they are doing
// tiered delivery while actually pouring every vulnerability into the group,
// with no indication that they misconfigured it. This kind of "silent
// downgrade" is exactly what should be caught at the entry point.
//
// Note that Validate is only for the **write** path. The read path still uses
// ParseFilter's lenient semantics, so bad values already in historical data do
// not make a channel unreadable as a whole.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("minimum severity %q is invalid; valid options: low / medium / high / critical, or leave empty for no limit", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel with the
// given filter conditions.
//
// **Never returns an error**, for the same reason as ParseFilter: any internal
// exception is treated as "match". Evaluation order: event type → severity
// threshold → task/asset scope → vulnerability-class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are only received by channels that explicitly opt
	// in. Default off, because the vast majority of users expect "push" to mean
	// "a new vulnerability was found", not a running log of every status
	// transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclude takes precedence: matching any exclude keyword disqualifies the
	// event, even if it also matched the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan over a small set is enough; both sides are on the order of
	// "a few dozen hand-picked values", where building a map costs more than it
	// saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of the keywords
// (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}