package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements the DingTalk custom robot.
//
// Platform characteristics (which determine implementation choices here):
//   - Single robot rate limit 20 messages/minute, oversending is silently dropped (HTTP may still be 200),
//     so rate limiting must be done client-side, see DefaultRatePerMin.
//   - Security settings choose one of three: signing / custom keyword / IP allowlist. Signing is the only option
//     that does not depend on message content, so only signing is supported (bare webhook with none enabled is also supported).
//   - Both success and failure return HTTP 200, distinguished by errcode in the body — not checking errcode
//     will record delivery failures as successes.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's Webhook URL contains an access_token, which is itself a credential, so it is masked entirely.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is the DingTalk Webhook URL itself; changing the URL requires re-asserting the signing secret for the new URL.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

// Send delivers a single message. When there is a back-link and it is a single item, uses ActionCard (with button), otherwise uses markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown body has no explicit byte limit, but we still cap it to prevent evidence fields from growing excessively.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside 200 responses.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is signature verification failure, 310000 is keyword mismatch — both are configuration errors,
		// retrying will not self-heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends timestamp and sign parameters to the webhook URL per the official signing rule.
//
// Rule: string to sign = timestamp + "\n" + secret, HMAC-SHA256's **key is also secret**,
// result is base64 then URL-encoded. timestamp is in milliseconds. When secret is empty, returns as-is,
// to support robots that do not have signing enabled.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Do not pass through err: url.Parse's error text contains the full URL (including access_token).
		return "", fmt.Errorf("failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL validates that the URL is usable, the protocol is supported, and checks literal IP targets for internal network access.
//
// Two considerations:
//
//  1. **Error messages must be redacted**. url.Parse returns *url.Error, whose Error() contains
//     the **full original URL**, and the URLs for these channels embed credentials (DingTalk access_token,
//     WeCom key, Telegram bot token, Feishu hook id). Previously this directly did `return err`,
//     so the "invalid URL format" error leaked credentials out to the test endpoint's 400 response,
//     the last_error column persisted on each delivery, server logs, and the delivery history API.
//
//  2. **Literal IPs are checked for internal network directly**, domain names are left for the dial phase
//     (blockInternalDial is the final enforcement point, and also covers DNS rebinding). Doing this check here
//     is to give a hint at config-save time rather than waiting for the first delivery to fail.
//
// Restricting protocols is defensive: file:///gopher:// etc. would cause http.Client to behave unexpectedly
// (though already blocked by the scheme check, there is no reason to open up this surface).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL cannot be resolved (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing delivery to local/link-local address %s (if you genuinely need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
