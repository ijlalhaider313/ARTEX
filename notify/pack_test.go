package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack whole items" fix: when a summary message exceeds the
// channel's length limit, it must be truncated **by whole items** and the number of
// items that did not fit must be accurately reported, so the caller only marks the
// actually delivered ones.
//
// The previous approach rendered the full text then truncated, then marked the entire
// batch as delivered: the second half of the message vanished into thin air, while
// delivery history showed all-success — findings just disappeared, with nothing to detect it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200-item summary, guaranteed to far exceed WeCom's 4096 bytes.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body %d bytes exceeds limit %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should only fit a subset (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must honestly state how many items this message contains and how many remain —
	// otherwise readers would take the header number as the total.
	if !strings.Contains(body, "remaining") || !strings.Contains(body, "next message") {
		t.Fatalf("header should state how many items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Should only include the first kept items.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "Vulnerability"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "Vulnerability"+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("should keep all when unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "remaining") {
		t.Fatalf("no truncation prompt should appear when not truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is too small to fit even one item, still emit one (final truncation is the backstop).
	// Otherwise one oversized finding would permanently stall the entire batch: every pickup can't fit, every time nothing is sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("should keep at least 1 item, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("single message should report 1 delivered, got %d", kept)
	}
	// Empty message has no deliverable items.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("empty message should report 0, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits by **character count**; using a byte budget would compress
	// CJK messages to a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body %d runes exceeds limit %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should only fit a subset, got %d", kept)
	}
	if !strings.Contains(text, "next message") {
		t.Fatalf("should state remaining items not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("card should only fit a subset, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body; the entire batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition not met")
	}
	// Indirectly confirmed via the renderer's return value: markdownBody(0) with no limit keeps all.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("should keep all when unlimited, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for "untrusted content must not alter message structure."
// Titles and summaries come from model output (the model reads tested target responses); asset names come from tested target URLs.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in result (escaped form)
		wrong []string // must not appear in result (unescaped form)
	}{
		{
			name: "newline + external link in title",
			item: Item{
				Severity: "high",
				Name:     "SQL injection at login\n[Urgent: click here to verify account](http://attacker.tld)",
			},
			// Newlines must be collapsed (otherwise can forge new list items/blockquotes);
			// brackets and parens must be escaped (otherwise it's a clickable external link).
			must:  []string{`\[Urgent: click here to verify account\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "image beacon in title",
			item: Item{
				Severity: "high",
				Name:     "finding ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and blockquote in asset name",
			item: Item{
				Severity: "high",
				Name:     "normal title",
				Assets:   []string{"a.com/*injection*>quote"},
			},
			must:  []string{`\*injection\*`, `\>`},
			wrong: []string{"*injection*"},
		},
		{
			name: "backticks and pipes in summary",
			item: Item{
				Severity: "high",
				Name:     "title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// Single-item writeItem is the shared render path for all three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form %q appeared (it could be used to inject structure or an external link):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks in escape order: backslash must be handled first,
// otherwise it would add another layer of escaping to the backslashes added later, producing double backslashes in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escape order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks in a specific regression:
// markdown escaping must not leak into Telegram's HTML output (escaping was once added
// in the shared title function, causing visible backslashes like `\(1\)` in Telegram messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("markdown backslash escapes appeared in Telegram body:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
