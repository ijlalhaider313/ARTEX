package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardenings:
//   ① a delivery address must not use the server as a jump box to reach the internal network / cloud metadata (SSRF)
//   ② address validation errors must not leak credentials embedded in the address
//
// About the test environment: many cases in this package use httptest fake receivers on 127.0.0.1, which the guard
// would block by default. So TestMain globally enables AllowLocalTargetsEnv, and each SSRF case below
// explicitly clears it to assert the **deny-by-default** behavior.

func TestMain(m *testing.M) {
	// Let regular cases reach the local fake receivers; SSRF cases will temporarily clear it themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core assertion of the SSRF protection:
// under the default config, delivery to a loopback address must be rejected at the **connection** layer.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // turn off the escape hatch = default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("delivery to a loopback address should not be allowed by default")
	}
	if hit {
		t.Fatal("the request already hit the local server — the guard did not take effect")
	}
	// The error message must guide the user on how to allow it (a local SMTP relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection message should explain how to explicitly allow it: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case: after explicitly opting in it must work,
// otherwise legitimate deployments like a local postfix / internal relay would be broken wholesale.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("should be able to deliver after explicitly opting in: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // cloud metadata endpoint — the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped form must be resolved back before judging, otherwise it is a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: self-hosted Mattermost / SMTP relays on the
	// internal network are common legitimate usage.
	// This assertion pins down that trade-off — if someone later adds a private-network check, this fails,
	// forcing a conscious decision (rather than silently breaking a set of deployments).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private networks are common legitimate delivery targets)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the front-end hint at config time:
// a literal IP should be rejected at save time, not wait for the first delivery failure.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at config time", raw)
		}
	}
	// Public and private addresses pass as usual (private ones are left to the dial stage, which does not block them).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials is the branch this audit found I missed last round.
//
// url.Parse **fails** returning a *url.Error whose Error() contains the full original address. Last round I only
// redacted the error returned by http.Client.Do and missed this one; and the "permanent failure path" cases I added
// (file://, gopher://, ftp://) all actually parse successfully and take the scheme branch, so being
// all green proved nothing about this path — a false assurance.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // invalid percent escape
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // port is not numeric
		"http://[::1?access_token=" + leakProbeToken,                  // mismatched brackets
	}
	for _, raw := range cases {
		// First confirm this input **does** make url.Parse fail. Without this step the case might
		// silently take a different branch (that is exactly how last round's false assurance happened).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should have failed to parse, otherwise this case does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel layer's wrapping also does not leak the address.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("an invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the dial guard for the SMTP channel.
//
// The email channel once used a bare net.Dialer, the only gap in the entire SSRF protection: setting host to
// 169.254.169.254 or 127.0.0.1 would connect directly, and when smtp.NewClient's handshake fails, the
// line returned by the endpoint is wrapped into the error and echoed back via last_error through the delivery
// history API — the same semi-blind read primitive other channels had already closed; the timing difference between
// "connection refused vs timeout" could also be used to probe ports.
//
// This package's TestMain globally enables AllowLocalTargetsEnv (many cases use fake receivers on 127.0.0.1),
// so this case must clear it itself — otherwise the test passes with or without the guard, which is
// exactly why the gap was never caught by any test.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // turn off the escape hatch = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("email delivery to a loopback address should not be allowed by default")
	}
	// The connection must never be established: the guard blocks in the Control hook, so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP session was established — the guard did not take effect")
	}
	// The error message must guide the user on how to allow it (a local postfix relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection message should explain how to explicitly allow it: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse case: after explicitly opting in
// it must deliver normally. Self-hosted SMTP / local relays are very common deployments, the guard must not be all-or-nothing.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("after explicitly opting in, local SMTP should be deliverable: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("no EHLO seen — the session was never really established")
	}
}
