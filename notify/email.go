package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout constrain connection establishment and the entire SMTP session, respectively.
// net/smtp itself has no timeout mechanism; without these two, a stuck endpoint would leave
// the delivery goroutine hanging forever — and the dispatcher is a single goroutine doing serial processing,
// which means the entire notification system would stall.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but should not be used for spam; a generous default value.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only mask the password. SMTP host, account, and recipients are not secrets; masking them only makes editing harder.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port determine which server the password is handed to; tls determines whether transport is encrypted. A change in any one
// requires re-asserting the password — and as a side effect makes "turning off TLS" a step that must explicitly carry credentials, rather than a casual toggle.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("missing SMTP server address")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("invalid SMTP port (should be 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("missing sender address")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("at least one recipient address is required")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: upgrade if the endpoint supports it. Credentials cannot be sent over a plaintext session (see auth below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over an unencrypted connection (unless the target is localhost).
			// This is the **correct** security behavior and must not be bypassed, but the cause needs to be clearly explained —
			// otherwise the user only sees "unencrypted connection" and does not know what to do.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials: connection is unencrypted. Enable TLS, or switch to port 465 (implicit TLS), or check the \"Enable TLS\" option (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP authentication failed: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("sender %s rejected", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("recipient %s rejected", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("failed to write email body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("failed to submit email: %w", err)
	}
	// Quit failing does not affect the fact that "the email has been accepted by the server", so its error is ignored.
	_ = client.Quit()
	// Email has no length truncation (the full HTML body is sent), so the entire batch counts as delivered.
	return len(m.Items), nil
}

// emailDial establishes the SMTP connection.
//
// implicitTLS=true uses the 465-style "TLS upon connect" approach; false uses 25/587 plaintext connect then
// STARTTLS. The two cannot be mixed: sending a plaintext greeting to port 465 will be immediately disconnected.
//
// The session deadline is set **at connection time** (not retrofitted later), because net/smtp's Client hides the underlying
// connection in an unexported field that is inaccessible externally; once the connection is handed over, only a pre-set deadline
// can serve as a backstop. This also covers blocking during the handshake phase.
// Control attaches blockInternalDial, sharing the same guard as the HTTP-based channels. Without it, SMTP
// would be a gap in the entire SSRF protection: setting host to 169.254.169.254 or 127.0.0.1 would connect directly,
// and when smtp.NewClient's handshake fails, the line returned by the endpoint would be wrapped in the error and echoed
// back via last_error through the delivery history API, forming a semi-blind read primitive; the timing difference between
// "connection refused vs timeout" could also be used to probe ports. The dial phase is the final enforcement point,
// and also covers DNS rebinding.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError classifies a stage failure as "retryable" or "permanent" based on the SMTP reply code.
//
// Why this distinction is necessary: SMTP 4xx and 5xx have completely different semantics —
//   - 4xx (450 greylisting, 451 local error, 452 insufficient storage) is a **temporary** rejection;
//     the correct approach is to retry later; especially with greylisting, nearly every first delivery hits this.
//   - 5xx (550 user does not exist, 553 address invalid) is a permanent rejection; retrying is pointless.
//
// If everything were classified as permanent, a mail server with greylisting enabled would cause **every** push
// to fall into failed after the first attempt — and those failures are exactly where automatic retry should shine.
// The reply code is taken from the first three digits of the error text; when no code can be parsed, it is treated
// as retryable (better to retry once than to kill a possibly transient failure because parsing failed).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode extracts the leading three-digit reply code from SMTP error text; returns 0 if not found.
// net/smtp does not export the error code field, so it can only be extracted from text; format is "450 4.7.1 ...".
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage assembles a complete RFC 5322 email.
//
// The body is base64-encoded for two reasons: first, SMTP requires no single line exceed 1000 bytes, and HTML
// bodies (especially summary emails) easily produce overly long lines; second, base64 inherently never produces
// lines starting with ".", avoiding the hassle of SMTP dot-stuffing.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// Non-ASCII subjects must be RFC 2047 encoded, otherwise clients will display them as garbled text.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length limit, so the body is not truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 folded at 76 characters, per RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
