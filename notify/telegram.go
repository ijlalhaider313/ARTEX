package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit is the character limit for Telegram sendMessage's text field.
const telegramTextLimit = 4096

// telegramChannel implements the Telegram Bot API.
//
// Platform characteristics:
//   - Authentication is entirely in the URL path (/bot<token>/sendMessage), no signing required.
//   - Uses HTML parse mode instead of MarkdownV2: MarkdownV2 requires escaping `_*[]()~`>#+-=|{}.!`
//     (18 characters total), missing one causes the entire message to be rejected; HTML only needs & < > escaped.
//   - Business errors are also hidden in HTTP 200 responses, determined by the ok field.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram allows about 1 message/second for private chats, 20 messages/minute for groups. Conservative value.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token is the full credential; chat_id is just the recipient, not a secret (without the Token, knowing it cannot send messages).
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url determines which API endpoint the Token is sent to (e.g. a self-hosted reverse proxy); changing it requires re-asserting the Token.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("missing Bot Token")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("missing Chat ID")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("invalid API URL: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse Telegram response: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 is rate limiting, backoff and retry is effective; the rest (400 param error, 401 token error, 403 blocked,
	// 404 chat not found) are configuration issues, retrying will not self-heal.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram rate limited: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram returned error %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint builds the sendMessage URL. When base_url is empty, uses the official API;
// when non-empty, used for a self-hosted Bot API reverse proxy (a common need under restricted networks).
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// Do not pass through err: the URL contains the Bot Token, and at this point even the addr should not be echoed.
		return "", fmt.Errorf("failed to build API URL (API URL: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML renders the HTML body, returning the body text and the actual number of items written (see Channel.Send).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram's limit is in **characters**, so packing is also measured by characters (runeSize).
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">View all in platform</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>Status change</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>Type</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>Assets</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>Summary</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">View details</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes is reserved for the message title and possible truncation hint (in characters).
const telegramReservedRunes = 160

// telegramBatchLine renders one item in the batch (unescaped; caller escapes uniformly).
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle renders the title line of a batch message. The count uses **the actual number of items included in this message**,
// not the total batch count — otherwise readers would think the number in the header is everything.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("Finding summary · %d total", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf(" (showing first %d, remaining %d continue in next message)", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("Last %d minutes · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape escapes HTML text content.
// Telegram only recognizes these three entities; after escaping, existing entities like &amp; will be double-escaped — this is exactly
// the expected behavior: we want to display the original characters, not let users inject HTML.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr escapes HTML attribute values. In addition to text escaping, it also handles quotes —
// quotes in URLs would prematurely close the href attribute, turning subsequent content into an injection point.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
