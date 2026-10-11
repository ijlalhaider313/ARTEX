package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing the result is
// valid UTF-8 and does not split a character.
//
// Why it must cut on a rune boundary: the WeCom group robot markdown has a
// hard 4096-**byte** limit (not a character count), and a Chinese character is
// 3 bytes. Slicing by bytes directly would cut a character in half, producing
// invalid UTF-8 — the platform would either reject the whole message or render
// it as a replacement-glyph block. The approach here is to walk back from the
// budget position to the nearest rune-start byte (utf8.RuneStart detects
// continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. An ellipsis is appended after truncation, unless max
// is too small to fit it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: drop the ellipsis and just
		// truncate, so the result does not exceed max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine collapses multi-line text into a single line: folds all whitespace,
// then truncates by character count. Used for the title line of IM messages —
// summaries often contain newlines that would break table/title layout when
// inserted directly. max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an
// ellipsis when truncated. max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's unit: WeCom limits by
// bytes, Telegram by character count. Using the wrong unit does not error; it
// just makes the message far shorter than expected (1 Chinese character = 3
// bytes, so a 4096-byte cut leaves only ~1365 characters), so both functions
// must be kept and chosen per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count, guaranteeing no
// half-cut tags are produced.
//
// Truncating HTML directly by characters can produce a broken tag like
// `<a href="htt`, which a platform parser will either reject outright or
// swallow the following body as an attribute value. The approach here: first
// truncate by characters, then check whether the tail has an unclosed `<`, and
// if so, retreat to before it.
//
// No tag balancing is done (e.g. closing </b>): Telegram's HTML parser
// auto-closes unclosed tags, and implementing balancing oneself would have to
// handle quotes inside attributes, comments, and self-closing tags — the
// complexity is not worth the payoff.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (a `<` after the last `>`),
	// retreat to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut-off HTML entity (e.g. `&amp;` cut to `&amp`), also
	// retreat. An entity fragment can cause a parser that only recognizes
	// entities to reject **the entire message** — an over-length digest message
	// is already common, and it is not worth losing the whole notification over.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items **fully** fit within the budget, for
// packing a digest message by whole items.
//
// Why pack by whole items rather than render-then-truncate: truncation makes
// the latter items vanish, while their delivery records are still marked as
// delivered — invisible in the message and invisible in the delivery history,
// so vulnerabilities just disappear. Packing by whole items leaves the items
// that do not fit in the database as the next batch, and the kept value the
// caller receives is the real count delivered in this message.
//
// Parameters: maxSize<=0 means no limit; reserve is the budget set aside for
// the message header/footer; size does the measuring (the unit differs per
// platform: WeCom/DingTalk count bytes, Telegram counts characters — using the
// wrong unit does not error but compresses Chinese messages far below the
// limit); render renders the idx-th item into its actual text — length varies
// by content and cannot be estimated.
//
// Returns at least 1 (as long as there are items). Even a single extremely
// long item must be sent — the caller's final truncation is the safety net —
// otherwise one over-long vulnerability permanently stalls the whole batch.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are the two measuring units for packItemCount, named so
// call sites don't end up with bare func(s string) int closures where it is
// hard to tell which unit is in use.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list into a single display line; when there are
// more than limit assets, it omits the rest and annotates the total count. A
// vulnerability can be anchored to dozens of assets; listing all of them would
// overwhelm the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " (" + itoa(len(assets)) + " total)"
}

// itoa is a short alias for strconv.Itoa, used only for assembling display text
// to avoid importing strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
