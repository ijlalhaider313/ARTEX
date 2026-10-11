package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets determines whether message delivery to loopback / link-local addresses is allowed.
//
// Rejected by default. These address ranges are not where IM bots or public email servers live,
// and what they can reach is sensitive: the admin port of another service on the same machine,
// and the cloud metadata endpoint (169.254.169.254, which can leak instance credentials).
// The delivery address is configured by an admin, but an admin session borrowed via XSS/CSRF,
// or a second person sharing the same JWT, can change the config and read the response back
// — doJSON writes the first 200 bytes of a 4xx/5xx response body into last_error, and the
// delivery history endpoint echoes it back, forming a semi-blind read primitive.
//
// However, "local SMTP relay" (postfix on 127.0.0.1:25) is a common setup for self-hosted
// email, and a blanket block would block users. So an explicit escape hatch is provided
// rather than a hardcoded allow: set ARTEX_NOTIFY_ALLOW_LOCAL=1 to permit it.
//
// Exported as AllowLocalTargetsEnv so tests can explicitly enable it — this package and the
// server package's test cases heavily use httptest fake receivers on 127.0.0.1, which would
// all be blocked by the guard without it.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP belongs to an address range that is
// "not allowed for delivery by default."
//
// Only loopback, link-local (including cloud metadata 169.254.169.254), unspecified,
// and multicast are rejected. RFC1918 private networks are **not rejected**: self-hosted
// Mattermost / SMTP relays on the internal network are common legitimate uses, and blocking
// them would make the feature directly unusable in real environments. This trade-off is
// deliberate — the guard blocks truly sensitive targets without disabling normal deployments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be reduced to IPv4 before checking, otherwise it bypasses the check.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook for the http.Transport dialer; it checks the
// target address **at connection time**.
//
// Why at the dial stage rather than only validating at config save time: this is the
// final enforcement point. It covers two cases that bypass config validation — DNS
// rebinding (resolves to a public IP at validation time, resolves to an internal IP at
// connection time) and redirects (although we already reject cross-host redirects,
// same-host redirects can still point the path elsewhere).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot parse target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refused delivery to loopback/link-local address %s (set %s=1 if you genuinely need to deliver to a local service)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds only a dial guard on top of the default Transport.
// Clone preserves all default tuning (connection pool, HTTP/2, timeouts, proxy, etc.),
// avoiding changing other behavior just to add a check.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the shared client for all channel deliveries.
//
// Deliberately **not** reusing the project's global egress proxy (GlobalProxy on the server
// side): that proxy is for pentest target traffic and is often an unstable tunnel, while
// notification availability should not be held hostage by target network jitter.
// IM pushes can go direct. Timeout is 15 seconds — anything slower is effectively a failure.
//
// Cross-host redirects are rejected: this feature's delivery addresses are all in the
// "one fixed endpoint" form and would not normally redirect to another host; and the
// credentials for these services (DingTalk's access_token, WeCom's key, Telegram's bot
// token) are **in the URL**, so following a cross-host redirect is equivalent to handing
// the credentials to the redirect target. Same-host redirects (e.g. appending a trailing
// slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refused cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit limits the size of the response body read. The peer may return a huge
// payload when something goes wrong, and we only need the status code and a short error
// description to show in delivery history.
const respBodyLimit = 8 << 10

// doJSON sends a request and returns the response body (length-limited).
//
// When payload is nil, an empty body is sent (for GET or platforms that don't require a body).
// Key-value pairs in headers are appended as-is, for custom headers in generic webhooks.
//
// Error classification is this function's core responsibility: network-layer failures and
// 5xx/408/429 are classified as "retryable," while other 4xx are "permanent failures" —
// retrying a 403 just logs the same error 3 times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failure is a local bug (config field type mismatch); retrying won't help.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Invalid URL — usually the user filled in the address wrong; a permanent failure.
		// Here too, err must not be passed through: url.Parse's error text contains the full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout — mostly transient; hand off to backoff retry.
		//
		// The error text must be redacted before propagating. Reason: http.Client.Do returns
		// *url.Error, whose Error() is `Op "fullURL": underlying error`, and this feature's
		// credentials are **in the URL** (DingTalk access_token, WeCom key, Feishu hook id,
		// Telegram /bot<token>/). Without redaction, credentials would flow through this error
		// string to four places: notification_deliveries' last_error (stored in plaintext),
		// the delivery history endpoint response (**bypassing channel config masking**),
		// server-side logs, and the 502 text returned to the frontend by the test-send endpoint.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limit) and 408 (timeout) are worth retrying; other 4xx are config or permission issues — retrying is pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("peer rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("peer service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("peer rejected request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses the response body into a one-line short text for error messages.
// The response may contain newlines and lots of whitespace; stuffing it directly into
// last_error would break the layout of the delivery history page.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses the delivery address to "scheme://host/…" for error messages.
//
// This is this package's only address redaction policy, deliberately made **coarse enough**:
// everything except scheme and host is discarded. The reason is that there is no "general
// and safe" way to determine which part of a URL is a credential:
//
//	DingTalk   credential in query         /robot/send?access_token=xxx
//	WeCom      credential in query         /cgi-bin/webhook/send?key=xxx
//	Feishu     credential in **path tail** /open-apis/bot/v2/hook/<hook_id>
//	Telegram   credential in **path mid**  /bot<token>/sendMessage
//
// To "keep only the useful part" would require per-channel patching, and missing any one
// is a credential leak. Keeping the host is enough for troubleshooting (DNS resolution
// failure, connection failure, bad certificate can all be located); which specific bot it
// is can be identified via the masked tail hint in the channel config.
//
// On parse failure, a fixed placeholder is returned — the raw string is never echoed back.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable address)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address from a transport-layer error, keeping only the underlying cause.
//
// *url.Error has the structure {Op, URL, Err}; its Error() prints the URL too.
// Here we explicitly extract the Err field, bypassing its Error() — more reliable than
// after-the-fact string replacement, because replacement must correctly handle various
// URL-encoded/escaped variants and is easy to miss.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error (e.g. errors returned by the redirect policy) may also carry an address; redact uniformly.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces http(s) addresses appearing in a text with their redacted form.
//
// Used as a fallback for errors where structured fields are unavailable (redirect policy
// errors, custom errors from third-party libraries). Only recognizes the http/https prefix
// and splits on whitespace and quotes — addresses do not contain these characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}