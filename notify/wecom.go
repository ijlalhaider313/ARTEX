package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard limit on WeCom group robot markdown content (bytes, not characters).
// This is the tightest limit among all six channels, and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the Enterprise WeChat (WeCom) group robot.
//
// Platform characteristics:
//   - The only authentication is the key in the URL; no signing is supported — so the webhook URL itself is the entire credential.
//   - The markdown content limit is 4096 **bytes**; exceeding it rejects the entire message (not truncation). Chinese is 3 bytes/char,
//     meaning the body can only hold about a thousand-some characters; client-side truncation is mandatory.
//   - Rate limit is 20 messages/minute, likewise enforced client-side.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential — the Webhook (key in the URL) — and it does not support signing,
// so the entire URL is the full credential; no other field needs masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only one field — the Webhook — which is both the destination and the credential, so there is no "residual credential after changing the address" concern.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A summary batch can be very long (50 items × one line each + prefix); 4096 bytes is easily exceeded.
	// Truncation is done here rather than relying on the platform error: being rejected means the entire batch is lost,
	// whereas truncation at least delivers the first several items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 is API call rate limit exceeded — the platform's rate limit window rolls forward,
		// so backoff and retry is effective; explicitly classified as retryable.
		// Reaching this point means the client-side rate_per_min is configured too aggressively;
		// retry is just a fallback — the real fix is to lower the channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 is invalid webhook key — permanent failure, retrying will not self-heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
