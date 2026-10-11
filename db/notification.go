package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// This file is the channel config and event layer for IM push notifications. Delivery task claiming and state transitions are in
// db/notification_delivery.go.
//
// Two invariants — must maintain when modifying this file:
//
//  1. The finding-write transaction (RecordFindingTx) only calls InsertNotificationEventTx for a blind insert,
//     reads no notification-related tables, does no filter matching. Any read operation introduced here could
//     pollute or even abort the finding-write transaction due to a user's misconfigured filter conditions.
//  2. Filter matching never errors: malformed configs are always treated as "match" (see notify.Match). Better to over-push
//     than to miss a push.

// ErrNotificationChannelNotFound channel does not exist.
var ErrNotificationChannelNotFound = errors.New("notification channel does not exist")

// Delivery states.
const (
	NotifyStatePending = "pending" // waiting to send
	NotifyStateSending = "sending" // claimed by a dispatcher, lease not expired
	NotifyStateSent    = "sent"    // delivered
	NotifyStateFailed  = "failed"  // retries exhausted or permanent failure, can be manually retried
	NotifyStateSkipped = "skipped" // channel disabled, no longer sending
)

// Push modes.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode whitelist-validates push modes (same rationale as findings.status: no DB CHECK,
// easier to extend later).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel is a channel instance config. Config and Filter are kept as raw JSON,
// parsing is delegated to the notify package — the db layer does not understand their field meanings.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled uses a pointer to distinguish "field not provided" from "explicitly false" —
	// the frontend toggle control only submits changed fields.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled returns whether the channel is enabled; when Enabled is nil (not loaded) it's treated as enabled.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is an event fact.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels returns all channel instances, enabled ones first, same level sorted by id.
// Sorting is done in SQL so the UI and dispatcher see the same stable order.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID gets a single channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel creates or updates a channel.
//
// On update, only fields explicitly provided by the caller (non-nil / non-empty) are overwritten, so the frontend can submit
// a partially modified drawer form without having to send back config fields it didn't display — sending them back would
// cause "masked value overwrites the real secret" accidents.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// Here we deliberately do **not** process 0 in any way: 0 is a valid config, meaning "no rate limit".
	//
	// This was previously written as `if c.RatePerMin <= 0 { c.RatePerMin = default }`, intended as
	// "give a safe default when unspecified", but that also swallowed "explicitly set to 0" — docs, UI hints,
	// and takeTokens all interpret 0 as no rate limit, but here it was silently changed to 20 (DingTalk/WeCom/Telegram)
	// or 100 (Feishu). Operators thought they had removed the rate limit but were actually throttled at 20/min with no indication.
	//
	// The distinction between "unspecified" and "explicit 0" is only known to the caller (field absent vs explicitly 0 in the request body),
	// so the default is filled by the server layer when the field is absent, see notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("rate limit value cannot be negative")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled toggles enable/disable.
//
// When disabling a channel, its unsent deliveries are also marked as skipped: otherwise after re-enabling
// it would suddenly receive a batch of "backlog during downtime" old findings — stale and easily misjudged as new.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "channel disabled", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel deletes a channel. Its delivery history cascades with the foreign key
// (channel config is gone, history is unreadable).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx **best-effort** writes a push event into the caller's transaction.
//
// This is the only notification-related change on the finding-write path: one INSERT, reads no tables, knows no channels,
// runs no filters. Transaction commit guarantees atomic consistency of "finding saved" and "push task exists" —
// there is no window where commit succeeds but no task was enqueued and the message is permanently lost.
//
// Two key design decisions, neither written casually:
//
//  1. **Why SAVEPOINT**: in PostgreSQL, any statement error inside a transaction puts the entire transaction into
//     aborted state, after which all statements (including COMMIT) fail. So "ignore this INSERT's error and let
//     the caller continue committing" is impossible in PG — unless you use a savepoint to isolate the error to
//     this one statement. Without a savepoint, "rollback the entire batch" is the only option.
//
//  2. **Why full rollback is wrong**: push is a convenience feature; the finding record is the product itself. A problem
//     in the notification table (unmigrated old DB, transient disk failure) should not prevent a high-severity finding
//     from being saved. So here we isolate the error, log it, return false, and let the finding write commit normally —
//     the cost is losing this one push. Returning bool instead of error is deliberate: the caller should not treat it
//     as an error that affects write success.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] serialize push event failed finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] create savepoint failed finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] write push event failed finding=%d (finding record is unaffected): %v", findingID, err)
		// Rollback to savepoint, rescue the transaction from aborted state.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] rollback to savepoint failed finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release the savepoint to avoid accumulating useless savepoints in long transactions.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is the standalone-transaction version of InsertNotificationEventTx, for call sites
// not inside an existing transaction (e.g. a channel's "send test message", which has no real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("serialize notification event snapshot failed: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands undispatched finding events into delivery tasks by currently enabled channels,
// returning the number of events processed and deliveries created this round.
//
// The entire round runs in one transaction: events are claimed with FOR UPDATE SKIP LOCKED, so multiple processes
// running concurrently each get different rows (the archive queue claiming in this project uses the same technique, see
// completeNextArchiveJob in db/task_archives.go).
//
// Filter matching is deliberately done in Go rather than SQL: a channel's filter is a JSONB of optional fields,
// expressing six combinations in SQL would make the query hard to maintain, and the number of channels is "a few hand-configured ones" —
// loading all and matching in memory is faster and easier to test.
//
// Events that match no channel are also marked fanned_out — otherwise they stay in the pending set forever
// and get rescanned every tick.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// The snapshot is our own write, theoretically always parseable; parse failure does not block the delivery flow,
		// but this event will be skipped by all channels with filter conditions due to all-empty fields — better to push one fewer
		// than let one bad row stall the entire queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind takes the inline value: the copy in the snapshot is for rendering and may have been written by an older version.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// Mark this round's events as dispatched. Events that matched no channel are also marked (see function comment).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx gets enabled channels within the transaction. The count is small,
// no pagination and no caching — caching would introduce an extra timing problem of "when does a config change take effect".
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames resolves asset ids into short display names for use in push messages.
//
// The return order matches the input and the length may be shorter (non-existent ids are skipped). Keeping the input order
// ensures stable asset order across multiple deliveries of the same finding — otherwise after a retry the asset order in the received message changes,
// which would be misread as "assets changed".
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName picks the most distinctive identifier by asset type.
// Returns empty string as fallback; the caller decides how to present "asset with no name" — this function does not fabricate placeholders,
// otherwise noise like "Asset#42" would get into push messages and readers might think it's a real domain.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify updates finding remediation status and registers a status change
// push event in the same transaction.
//
// Returns from=previous status; found=whether the finding exists; notified=whether the event was registered successfully.
//
// Three deliberate behaviors:
//   - No event is registered when the status hasn't actually changed. Duplicate submissions of the same value from the frontend drawer,
//     or idempotent replays by automation scripts, should not produce push noise.
//   - When the finding doesn't exist, returns found=false with no writes; the caller translates this to 404.
//   - Event registration failure does not affect the status update (see RecordNotificationEventTx's savepoint notes),
//     so when notified=false the status has already been updated successfully; the caller should not error on this.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx updates finding status and registers a status change push event within the **caller's transaction**.
//
// Extracted as a transaction-level function so all status-changing paths share the same semantics — previously only
// patchFinding used the notification-enabled version, and **when a retest concluded "fixed"** (that
// `UPDATE findings SET status=...` in finding_retests) it wrote directly to the db, so channels configured with
// `on_status_change` got no push at all for this status transition: the status silently changed in the UI,
// and ops had to open the platform to find out.
//
// Returns from=previous status, found=whether the finding exists, changed=whether the status actually changed,
// notified=whether the event was registered successfully (registration failure does not affect the status update, see RecordNotificationEventTx).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// If the status didn't actually change, don't register an event: duplicate submissions of the same value,
		// idempotent replays should not produce push noise.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats is the overview counter at the top of the notification page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // milliseconds since the oldest pending delivery
}

// NotificationStatsSnapshot summarizes the notification system's health.
// BacklogAgeMS is the most direct indicator of "is the push stuck" — far more useful than the pending
// count, because the difference between 3 backed-up items and 3 backed-up items can be 3 seconds vs 3 hours.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
