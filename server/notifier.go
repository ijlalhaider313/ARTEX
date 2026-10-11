package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global setting keys (stored in the settings key-value table, no table creation needed).
const (
	// settingNotifyEnabled is the master push switch. On by default: it is for one-click triage during maintenance,
	// not an enablement condition for the feature — the real enablement condition is "whether any channel is configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external address used to generate the finding-detail back link
	// (e.g. https://artex.example.com). Leave empty to omit the back-link button from messages.
	// The project has no reusable external-address config, so a new entry was added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest-mode interval (minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. 3 seconds is that engine's real-time ceiling,
	// and the main source of latency between "finding persisted" and "message reaches IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration taken when claiming deliveries. It must be significantly longer than the
	// worst-case time of a single delivery (the notify package's HTTP client times out at 15 seconds), otherwise
	// the same row can be delivered by two dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits how many events are fanned out per round, to avoid expanding the entire
	// historical backlog into delivery tasks all at once when a channel is first enabled.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest interval.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no rate limit.
	// Its purpose is to prevent "one channel configured as unlimited + a single scan turning up thousands of
	// findings" from dragging a single loop round into a long block.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the maximum number of deliveries per channel per round.
	//
	// This cap is derived backwards from **lease duration**: claiming stamps rows with a lease (notifyLease = 3 minutes),
	// so if one round serializes so many deliveries that the worst-case time exceeds the lease, the later ones expire
	// before they are even sent. Within a single process this is harmless (Run is one goroutine running serially, and a
	// tick never re-enters), but with **two processes connected to the same DB**, the peer re-claims and re-sends rows
	// whose lease expired, double-increments attempts, and judges them failed while the original process is still delivering.
	//
	// Value: 3-minute lease / 30-second per-send timeout = 6 means **exactly filling the lease** with zero headroom,
	// so it is not usable; 5 leaves 30 seconds of headroom out of a worst-case 150 seconds. This relationship is pinned
	// by TestNotifyTickBudgetFitsWithinLease — changing notifyLease, notifySendTimeout, or this value would make that
	// assertion fail.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout of a single delivery. It also determines the value of the previous constant;
	// the two multiplied together must not exceed notifyLease, see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failed retries, indexed by attempts already made.
// The 3 chances (including the first) correspond to db.MaxNotifyAttempts; the two must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for finding pushes.
//
// It runs as an independent goroutine alongside Scheduler (see server.New). It deliberately does not reuse
// Scheduler's tick: push real-time requirements (3 seconds) differ from the trigger's business cadence, and
// the two failures are independent of each other — a stuck push must not affect agent triggering.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. There are few channels and little contention, so a single mutex is enough;
	// it is not worth introducing a finer-grained structure for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the token bucket of a single channel.
//
// A token bucket is used instead of a "count per minute then reset" sliding window because the latter has terrible
// boundary effects: sending 20 at the end of a window and 20 more the next instant looks like 40 in one second to the
// platform and gets rate-limited; the token bucket refills at a constant rate and naturally avoids such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: first fan out new events, then deliver due tasks.
//
// Any step failure is only logged and does not interrupt the loop — a notification-system fault must never escalate
// into a process-level problem. Each tick is independent; the next round retries naturally.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to fan out events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket's unit of measure is **message count** (equivalent to HTTP request count), not finding count.
		// In realtime mode the two are the same (one finding, one message); in digest mode a whole batch of findings is
		// combined into one message, so it consumes only one token.
		//
		// Both modes ask the token bucket first, then claim up to the quota — the order cannot be reversed, otherwise a
		// delivery blocked by rate limiting has already consumed retry attempts.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns the digest channel's token consumption and its batch-size upper bound for this round.
//
// The two return values are **two different dimensions**, which is exactly why this is its own function:
//
//   - tokens is the message count. A batch of findings becomes one message and one HTTP request, so it is always 1.
//     rate_per_min therefore still applies to digest (at most this many digest messages per minute).
//   - claimLimit is the maximum number of findings in this batch. It is bounded only by memory, unrelated to the
//     request budget.
//
// Once, to make rate_per_min apply to digest, the per-round request budget
// (notifyMaxSendsPerChannelPerTick, derived backwards from the lease) was passed down directly as the batch size.
// The consequence: a channel with rate_per_min=20 only refills 1 token in a 3-second tick, so each digest message
// carried only 1 finding — digest degraded into "realtime push with digest wording", and readers got a string of
// "1 new finding in the last 30 minutes", while db.MaxDigestBatchSize was never reachable.
//
// This symptom is hard to spot in end-to-end tests (the existing cases all manually pass a large-enough limit to
// stepDigest, bypassing the quota math in step), so the decision is gathered here and pinned directly by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers a channel's realtime tasks, one finding per message.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim realtime deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A render failure is a local data problem; retrying will not make it better.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to judge digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries with broken snapshots that failed to make it into the message must be explicitly failed. Otherwise
	// they stay outside included, entering neither the message nor the failure list — on a successful send their state
	// gets missed by the later batch mark, stuck at sending forever until the lease expires and they are re-claimed
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "the event snapshot could not be parsed, so this finding could not be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad-snapshot deliveries failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Only the ones that made it into the message are handed to send: included[i] strictly corresponds to
	// msg.Items[i], and send relies on that correspondence to map "the channel reported it packed the first K items"
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and advances state according to the result.
//
// Deliveries in the same batch (possibly dozens in digest mode) share one send result: either delivered, or the whole
// batch retries. There is no per-item retry — a digest message is one message, and resending part of it would scramble
// the batch semantics.
//
// The only exception is **segmentation caused by the channel length cap**: if the channel actually packed only the first
// K items, then from item K+1 onward must be left for the next batch, not marked successful along with the rest.
// Otherwise the truncated findings are in neither the message nor the failure list, and disappear entirely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap a single delivery so a stuck channel does not drag down every remaining channel this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The channel cannot report more items than the delivery count; if it does, the render layer miscounted —
			// treat as all delivered and log the problem, which beats scrambling the records.
			log.Printf("[notify] channel reported %d delivered, exceeding the %d deliveries channel=%s; treating as all delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message hit the channel length cap: the rest go back to the queue immediately and the next tick
			// continues sending. Use DeferDeliveries rather than RescheduleDeliveries — this is not a failure, so it
			// must not consume the retry budget (the claim already optimistically +1'd, and that is where it gets
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message hit the channel length cap; only the first %d were delivered, the rest are left for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to enqueue segmented continuation channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither errored nor said how many were delivered. Treat as failure (go through backoff),
		// so this delivery is not repeatedly claimed but never marked off.
		err = fmt.Errorf("channel did not report delivered count (delivered=%d)", delivered)
	}

	// Failure disposition is decided **per item**, not by judging the whole batch's maximum attempt count.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts` condemning the whole batch, but each item in the
	// batch has a different attempt count: an old delivery that already retried twice (attempts=2) would drag a fresh
	// delivery in the same batch (attempts=1) into failed together — a new finding permanently lost without a single
	// retry, exactly the opposite of "don't let old rows drag new rows down".
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] failed to mark failure state channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] failed to mark failure state channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Regroup by delay: there are only 3 backoff tiers, so the number of groups is naturally small; no need to issue one
	// UPDATE per item (that would turn a 500-item batch into 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d retries-exhausted=%d pending-retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in all that are not in keep (compared by pointer identity).
// Used to find deliveries that "did not make it into the message" — they must be disposed of explicitly, not left in
// limbo.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt fetches the channel implementation and parses its config.
// Returning ok=false means the kind is not registered; the delivery should be failed directly instead of retried forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On config-parse failure, give an empty map: the channel's own Validate will report "which field is missing",
		// and that error guides the user's fix better than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single-finding message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders a digest message. Snapshots are parsed one by one — a single broken one only skips that one,
// without sinking the whole digest batch.
//
// The returned included corresponds **strictly one-to-one** with msg.Items (the i-th delivery ↔ the i-th item).
// This correspondence is a hard requirement: the caller decides the first K deliveries are marked delivered based on
// "the channel reported it packed the first K items". If a broken snapshot were skipped here without also dropping the
// skipped delivery from included, the indices would misalign — the bad item that should fail gets marked delivered,
// and the good item is wrongly treated as undelivered. The broken ones are explicitly marked failed by the caller, see
// stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A bad snapshot goes into neither the message nor included — its disposition is the caller's job
			// (explicitly marked failed, not slipped through among the "delivered").
			log.Printf("[notify] skipped unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch are unparseable", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a pending push item, also resolving asset names and the detail back link.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Asset-name resolution failure must not block the push: missing names is far lighter than missing the
		// notification; the message just loses one asset line.
		log.Printf("[notify] failed to resolve asset names finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// The detail-page route is web/src/app/(main)/function/findings/detail/page.tsx,
		// which reads the finding id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want** tokens from a channel's token bucket, returning how many were actually taken.
//
// One token = one message (one HTTP request). In realtime mode the caller passes as many as it wants; in digest mode a
// whole batch of findings sends only one message, so it passes 1.
//
// The bucket capacity is the channel's per-minute cap, refilled at a constant rate. ratePerMin<=0 means no rate limit;
// a finite but large-enough value is returned to keep a single loop round from being dragged down by unlimited backlog.
//
// The want cap is essential: without it the only option is to drain the whole bucket, yet the caller also has its own
// per-round cap — the extra tokens are unusable and vanish before the next refill, so accumulated burst capacity is
// never reachable, and even "this round has no pending deliveries" would still deduct one.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill by the real elapsed time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: token counts are accumulated in floating point, and refilling in two steps
	// can make 0.5 + 0.5 come out as 0.9999999999, which a direct int() would truncate to 0 — a mathematically full
	// bucket yielding no token. 1e-9 is far smaller than one token, so it never lets a real deficit slip through.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for back links, with any trailing slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest interval, falling back to the default when invalid or unset.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of the event corresponding to a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("delivery %d has an empty event snapshot", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse delivery %d's event snapshot: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event kind is authoritative from the event; the snapshot copy may have been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
