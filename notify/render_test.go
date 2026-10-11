package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most important invariant in this package. WeCom limits by **bytes**, and a multi-byte
	// character (e.g. CJK at 3 bytes/char, emoji at 4 bytes) would be sliced in half by any hard byte-cut
	// implementation, producing invalid UTF-8 that the platform rejects.
	// Use several inputs of coprime lengths to hit every possible cut point.
	inputs := []string{
		"Chinese test content",
		"mixed content text",
		"aCbHcIdNeSe",
		"🔴🟠🟡🔵", // 4-byte emoji, a bad cut is more obvious
		strings.Repeat("flaw", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result %d bytes exceeds the limit", in, max, len(got))
			}
			// When not truncated, the content must not be modified.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: modified the content even though it was not over the limit -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean unlimited")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean unlimited")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, appending the ellipsis must not push the result back over the limit.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("with max=1 the result %q has length %d over the limit", got, len(got))
	}
	// The normal case should carry the ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected an ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The difference in measuring units vs TruncateBytes must be preserved: Telegram limits by character,
	// and a byte budget would compress a CJK message to a third.
	s := "abcdefghij"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string under a byte budget should be noticeably shorter.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("the byte budget should not produce the same character count as the character budget")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("first line\n\nsecond line\twith tab   multiple spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("should collapse all whitespace, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("should not keep consecutive spaces, got %q", got)
	}
	// After truncation it must still be readable and valid.
	got = OneLine("abcdefghij", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Cutting HTML directly produces fragments like `<a href="htt`, and the platform rejects the whole message.
	s := `<b>Title</b>body body body<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result %d characters over the limit", max, n)
		}
		// The tail must not have an unclosed `<` (i.e. a `<` with no `>` in the last segment).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: a tail tag was cut in half -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("under the limit should list all, got %q", got)
	}
	// When over the limit the total count must be annotated, otherwise the reader does not know
	// how many assets were omitted.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "(5 total)") {
		t.Fatalf("should annotate the total of 5, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("empty severity has ordinal 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("an empty threshold should allow through")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("unexpected status label, got %q", got)
	}
	// Unknown statuses are echoed verbatim, no label is invented.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("unknown status should be echoed verbatim, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers an omission this audit found: truncation must avoid not only
// half-cut tags but also HTML entities cut in half.
//
// After `&amp;` is cut into `&amp`, a parser that only recognizes entities may reject the **whole** message —
// and over-long summary messages are already common, so the cost is too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain an entity fragment with an `&` but no matching `;`.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: entity fragment left at the tail %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity appeared", max)
		}
	}
}
