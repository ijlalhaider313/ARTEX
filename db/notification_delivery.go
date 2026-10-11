package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file handles claiming delivery tasks and their state transitions.
//
// Claiming uses a "lease" rather than a long transaction: the row is set to
// sending and next_attempt_at is pushed into the future as the lease expiry,
// then the transaction commits before the network delivery is performed. This
// way no database lock is held during delivery — network requests can take
// seconds (client timeout 15s), and holding a row lock would drag down other
// writes on the same database.
//
// The cost is that if the process crashes mid-delivery, the row stays in
// sending. This is **self-healing**: once the lease expires next_attempt_at
// falls into the past, and the next claim cycle picks the same row up again
// (see the claim condition's state IN ('pending','sending')). The retry count
// is already +1 at claim time, so a crash does not cause infinite retries —
// after MaxNotifyAttempts chances are exhausted it falls into failed for manual
// handling.

// MaxNotifyAttempts is the maximum number of delivery attempts (including the first).
// Defined here rather than in the delivery engine: it is the state machine's own
// policy; the engine is just the executor.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize is the maximum number of deliveries a single digest batch
// merges at once.
//
// The reason it exists is resources: if a digest cycle discovers tens of
// thousands of findings (entirely possible — a single full scan can do it),
// without an upper bound the claim would read every row into memory, render one
// giant message, and then have most of it truncated by the channel's length
// limit — wasting memory and **silently dropping** the truncated findings.
// With a ceiling, the overflow stays in the database as the next batch and goes
// out naturally in the next cycle, never lost.
//
// The basis for 500: it is the order of magnitude that, after rendering into a
// message, still has "readable content" within the WeCom 4096-byte limit; any
// larger just moves where the truncation happens.
const MaxDigestBatchSize = 500

// NotificationDelivery is one delivery task, including the channel config and
// event snapshot needed for rendering.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// Jointly loaded render context, not serialized (the server layer assembles the DTO).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind carried from the event, for jumping to finding details from the history list.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind are redundant fields for list display, saving the frontend a second query.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery is the unified read shape for delivery rows: delivery + event snapshot + channel config.
// All three are required to render a message; querying them separately would mean three round-trips.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery describes one claim cycle: select candidates by sel and lock them,
// then set them to sending and extend the lease. The lease placeholder in sel is
// left as $n for the caller to supply.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries claims a batch of due realtime deliveries for one
// channel, up to limit rows.
//
// It deliberately claims per **single channel** rather than "globally claim a
// batch then pick what to send": the rate limiter in the delivery engine is
// maintained per channel, so only by first knowing how many more this channel
// can send this round and then claiming exactly that many rows will rate
// limiting not burn retry attempts. If it claimed first and discarded later,
// the rows blocked by the limiter would already have attempts +1, and the
// 3-attempt budget would be consumed by pure waiting, landing in failed.
//
// The condition includes "sending with an expired lease" — that is the
// crash-recovery landing point. The lease must be significantly larger than the
// worst-case delivery time (channel HTTP client timeout 15s), otherwise the
// same row would be delivered by two dispatchers simultaneously. Disabled
// channels are also filtered out: the disable operation already marks existing
// deliveries as skipped, and this adds another guard against stragglers during
// concurrent disable/claim.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue reports whether a channel has accumulated a due batch: there
// are pending deliveries, and **the oldest one** is at least as old as the digest
// cycle.
//
// The check is based on the oldest delivery's age rather than wall-clock time:
// this way a freshly created channel does not immediately spit out a one-item
// "digest" just because it aligned to the hour, and a long-backlogged batch does
// not wait an extra cycle for nothing.
//
// It is separate from ClaimDigestBatch because the semantics differ: this
// function only answers "should we send?", while the claim takes **all** pending
// rows for the channel (including those not yet old enough) — otherwise one
// cycle would be split into multiple messages, defeating the point of digest.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch claims all due pending deliveries for a channel as one digest
// batch, up to MaxDigestBatchSize rows.
//
// All deliveries in the same batch share a batch_id, using the smallest id in
// the set as the batch number (stable, readable, no extra sequence needed). On
// retry, COALESCE preserves the original batch number so "these N were sent
// together" still holds after multiple retries.
//
// Rows are taken by ascending id rather than randomly: the earliest deliveries
// are sent first, so under backlog there is no starvation where new findings go
// out first and old ones are stuck behind forever.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit is a **memory ceiling**; the caller passes MaxDigestBatchSize, and
	// this clamps it again to guard against a caller passing a larger value.
	//
	// It deliberately does not accept the "rate-limit quota" as the batch size:
	// the rate-limit unit is messages — one batch sends one message and consumes
	// one token, deducted by the server layer's takeTokens — which is a
	// different dimension from "how many findings fit in one batch". Once, to
	// make rate_per_min apply to digest, the per-round request budget was passed
	// in as the batch size; the result was that a rate=20/min channel packed
	// only 1 finding per batch, and digest degenerated into realtime push with
	// digest wording. To change rate limiting, change takeTokens' want; do not
	// touch this.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries performs "select + set to sending and extend lease + read full rows",
// all in one transaction. postClaim is an optional extra step (digest batches use
// it to write batch_id).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Set to sending and push next_attempt_at into the future: that future moment
	// is the lease expiry, so "lease not expired" and "not yet retry time" share
	// the same conditional expression without needing a new column.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent marks a batch of deliveries as sent.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries returns a batch of deliveries to pending and pushes back
// the retry time.
//
// Returning to pending rather than introducing a new intermediate state keeps
// "how many chances are left" expressed in one place (MaxNotifyAttempts),
// preventing the state machine's branches from bloating with retry policy.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries returns a batch of deliveries to pending, immediately
// re-claimable, and **reverts the attempt counted at claim time**.
//
// It has one use case: when a digest message is sent in segments due to the
// channel's length limit, entries that did not fit in this message must stay
// for the next batch. That is not a failure, so it must not consume the retry
// budget — attempts was optimistically +1 at claim time, so it must be
// decremented back here. Otherwise a 500-item backlog split into 25 segments of
// 20 would have tail entries judged failed by MaxNotifyAttempts at segment 3,
// even though they never encountered any error.
//
// GREATEST(...,0) guards against "someone manually retried and zeroed attempts
// then ended up here again", preventing the count from going negative.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries marks a batch of deliveries as permanently failed, awaiting
// manual retry from the delivery history.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// Placeholders start at $3: $1 is state, $2 is last_error.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery manually retries one delivery: reset to pending,
// zero the retry count, set immediately due. Zeroing the count is deliberate —
// a human clicking "retry" means the previous failure cause has been addressed,
// so limiting it by the old count would make no sense.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery %d does not exist or its current state does not allow retry", id)
	}
	return nil
}

// NotificationDeliveryFilter is the query condition for delivery history.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries returns delivery history with pagination, newest first.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError truncates the error message to the column's accepted length.
// Channel response bodies can be very long
// (especially when a generic Webhook hits a self-hosted service); not truncating
// would bloat the history list's payload.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// Fall back to a character boundary to avoid leaving half a UTF-8 character
	// that would render as garbled text on the frontend.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders generates a $n placeholder string starting from start, plus the
// corresponding args, for use in IN (...).
// e.g. start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
