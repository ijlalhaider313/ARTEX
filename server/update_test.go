package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache is the layer that protects GitHub quota: the unauthenticated API
// allows only 60 requests/hour/IP, and the top bar's "new version available" hint
// checks once per full page load. Once the cache expires, a user with several open
// tabs can drain the quota — after which a real update check can't get through.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 queries should only hit source once, got %d", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// User clicking "Check for updates" must get a live result, otherwise a freshly published version won't show until the cache expires.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass cache, expected 2 source hits, got %d", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Roll the persisted time forward to just-expired, simulating TTL elapse.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("after TTL expiry should re-hit source, expected 2, got %d", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github unreachable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected error returned")
	}
	// Failed results must also be cached briefly, otherwise when GitHub is unreachable every page load waits for a timeout for nothing.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected error returned")
	}
	if calls != 1 {
		t.Errorf("errors should be short-cached, expected 1 source hit, got %d", calls)
	}

	// But the error TTL must be significantly shorter than the success TTL, so the system self-heals quickly after network recovery.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("error TTL(%v) must be shorter than success TTL(%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected error returned")
	}
	if calls != 2 {
		t.Errorf("after error TTL expiry should retry, expected 2, got %d", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// A visitor closing the tab cancels the request. That doesn't mean GitHub has a problem, and "cancelled" must never be written
	// to the cache — otherwise every visitor in the next 30 minutes gets a baffling error.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // expire the cache, force a source hit

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("when caller has already cancelled, the error should be passed through")
	}

	// Key invariant: the cancelled attempt leaves no trace — the cache holds neither the "cancelled" error
	// nor anything other than the previous good result.
	if c.err != nil {
		t.Fatalf("cancel error should not be written to cache, got %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("cache should retain the previous good result, got %+v", c.rel)
	}

	// That cancellation produced no new data, so the next visitor should re-hit source — and get a normal result,
	// unaffected by the previous cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("normal request after cancellation should not error: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("should get normal result, got %+v", rel)
	}
}
