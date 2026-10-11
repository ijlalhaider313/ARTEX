package notify

import (
	"fmt"
	"strings"
)

// This file is the shared message rendering for the "Markdown family" of
// channels (DingTalk, WeCom). Feishu uses card JSON, Telegram uses HTML, and
// email uses HTML; each renders in its own adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A
// vulnerability can be anchored to dozens of assets; listing all of them would
// overwhelm the message with no information value — nobody looks at the 5th
// domain onward in an IM client.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. An IM
// message is a "prompt to go look at the details", not the report body; the
// full content lives in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is the budget reserved for the message header (summary
// line + severity distribution + possible truncation note) and footer (platform
// link). When packing by whole items, this portion is deducted from the budget
// so the header and footer are never truncated — once they are cut, the reader
// cannot even tell "which batch this is, how many items are not shown".
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why this is necessary: vulnerability titles, summaries, classes, and asset
// display names all come from **untrusted sources** — titles and summaries come
// from model output (the model reads the target's responses), and asset URLs
// are full URLs obtained by scanning (including target-controlled query
// strings). Without escaping, a vulnerability titled
//
//	Login SQL Injection\n[Urgent: click to verify account](http://attacker.tld)
//
// would render as a **clickable external link** in a security engineer's
// DingTalk/Feishu; and `![](http://attacker.tld/beacon)` would be fetched by
// the client at render time, effectively announcing "this vulnerability has
// been viewed" and leaking the viewer's IP. Even non-malicious content, bold
// text or blockquotes injected could push the serious vulnerabilities below
// out of the fold.
//
// The escape set covers characters that change structure or produce clickable
// elements: title/link/emphasis/list/quote/strikethrough. `\` must be processed
// first, otherwise it would re-escape the backslashes added later.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText collapses untrusted text into a single line and escapes it, for
// use in markdown bodies. Single-lining is the other half of escaping: a
// newline by itself can forge a new list item or blockquote, which escape
// characters cannot stop.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the title bar/card title on IM
// platforms); the content is **unescaped original text**.
//
// Escaping is deliberately not done here: this title is shared by renderers in
// four contexts — markdown body, Telegram HTML, Feishu card plain_text, and
// generic Webhook JSON / email subject. Each context has different escaping
// rules (markdown escapes in HTML leave visible backslashes; in JSON they
// corrupt the data), so escaping must be handled by each output end. See
// writeItem / feishuItemLines / telegramEscape. Markdown escaping was once
// added to this shared function, and Telegram messages ended up showing visible
// backslashes like `\(1\)`.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability digest · %d items", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and the **actual
// number of items written**.
//
// The returned kept is the number of items truly delivered in this push; the
// caller marks only the first kept items as delivered — items blocked by the
// channel's length limit must wait for the next batch rather than being marked
// successful along with the rest. This is the source of "silent loss": the
// message was truncated, but the delivery record shows everything delivered,
// and nowhere can you see that the second half was never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even if over-length (the final truncation
		// is the safety net): partial information about one vulnerability is
		// better than sending nothing at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the digest message header: the time window, item
// count, and severity distribution. With these, the recipient of a digest can
// tell whether this batch needs immediate attention without opening the
// platform.
//
// items is the set of items **actually packed**; total is the count the batch
// should have. When they differ, the header must explicitly state "how many
// items are in the next message" — otherwise the reader assumes the number in
// the header is the total, while the items that were never sent do not exist
// anywhere in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new findings in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new findings**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (showing the first %d; the remaining %d continue in the next message)", len(items), extra)
	}
	// Severity distribution lets the reader see at a glance whether there are
	// any critical items. Only counts the items **actually included** in this
	// message, so "Critical 3" matches the items that can be counted below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single vulnerability entry.
//
// prefix is the sequence number for digest lists; single=true renders the full
// version (with summary and detail link), while the digest list renders only a
// one-line summary — otherwise a 50-item digest would become a long document.
//
// All content from external sources (title/class/assets/summary) goes through
// markdownText: single-line + escape. The detail link is assembled from the
// admin-configured public_base_url; it is not untrusted content and must be a
// clickable link, so it is output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: single-line presentation, with assets and a compressed
		// summary following.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}