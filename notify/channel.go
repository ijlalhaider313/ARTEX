package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is the adapter for a notification channel. Implementations must be
// **stateless**: the same instance is reused concurrently by multiple channel
// configs, and credentials are always passed in via the cfg parameter.
type Channel interface {
	// Kind returns the channel type identifier; it must match the registry key.
	Kind() string
	// Validate is called when saving config, checking required fields and
	// formats. The returned error is shown directly to the configurator, so the
	// message should state "which field is missing" rather than a generic
	// "invalid config".
	Validate(cfg map[string]any) error
	// Send delivers one message, returning the **actual number of items
	// delivered** and an error.
	//
	// Why return a count: every platform has a message length limit, and a
	// digest message that cannot fit the whole batch gets truncated. If the
	// caller unconditionally marks the whole batch as delivered, the truncated
	// items simply vanish — invisible in the message, shown as successful in
	// the delivery history, with nowhere to discover that a vulnerability was
	// never sent. By returning kept, the caller marks only the first kept items
	// and leaves the rest for the next batch.
	//
	// A non-nil error means delivery failed; *PermanentError means it should
	// not be retried. On failure kept is meaningless and the caller should
	// ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the channel's officially recommended per-minute
	// delivery cap, used as the default rate limit when creating a new channel
	// instance. Returning 0 means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the keys in this channel's config that are credentials.
	// When the API echoes config, these keys' values are masked; when an update
	// receives a masked value, the stored value is preserved. Only the
	// implementation itself knows which fields are credentials (for WeCom the
	// entire Webhook URL is the credential, while for DingTalk it is only the
	// secret within it), so this knowledge must come from the channel, not be
	// guessed by upper layers.
	SecretKeys() []string
	// DestinationKeys returns the keys in this channel's config that decide
	// "where the message is sent".
	//
	// Like SecretKeys, this is security-relevant: destination and credentials
	// are two independent sets of fields. If "change only the address, keep the
	// credentials as-is" were allowed, anyone who can edit channel config could
	// send the real credentials from the database to a server they control,
	// making the channel config masking completely meaningless.
	// See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. Deliberately an explicit literal rather
// than init() self-registration: this way "which channels exist" is visible in
// one place, and adding a channel surfaces omissions at compile time rather
// than relying on runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get retrieves the channel implementation by kind.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (for
// stable display in a UI dropdown).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried:
// credential errors, destination rejection, malformed request body, etc.
// Retries only help with transient faults (network jitter, rate limiting,
// upstream 5xx); repeatedly backing off a permanent failure never succeeds and
// drowns the real error in retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil,
// convenient for writing `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the err chain carries a permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- config-reading helpers ----
//
// Channel config comes from a database JSONB column; after encoding/json
// deserialization it is map[string]any, where numbers are always float64 and
// arrays are []any. The helpers below unify this conversion and tolerate the
// type drift that happens when a user leaves a field blank in the UI (e.g.
// entering the port as a string).

// cfgString reads a string config entry, trimming leading/trailing whitespace
// — copy-pasting from a web form often carries it in.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an integer config entry, accepting both float64 (the JSON
// default) and string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a boolean config entry, accepting string "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-array config entry, auto-trimming whitespace and
// discarding empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accept a single string, for form submissions that have only one
		// value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map config entry (e.g. custom HTTP headers), trimming
// whitespace on both keys and values and discarding empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}