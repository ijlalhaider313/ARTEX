package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv lets the subprocess spawned by the smoke test skip Bootstrap entirely.
//
// Strictly speaking it would be safe without this: the subprocess's
// os.Executable() is artex.new, so all derived paths carry the .new prefix and
// can't touch the real upgrade files. But relying on that coincidence is too
// fragile; an explicit short-circuit is clear and saves the subprocess a
// pointless disk probe.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is the instruction Bootstrap gives to main.
type Action int

const (
	// Continue: start the server normally.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart so the watchdog script restarts us.
	Restart
)

// State describes the upgrade state at this startup, so /api/update/check can
// truthfully tell the frontend "the last upgrade succeeded or was rolled back".
type State struct {
	Pending     bool   // swapped but not yet confirmed stable
	RolledBack  bool   // automatic rollback was just performed on this startup
	FailedStage bool   // staged file failed verification/smoke test, discarded
	Detail      string // one-line user-facing explanation
}

// Bootstrap runs at the very beginning of main, before any port listening or
// database opening.
//
// Three scenarios:
//
//	① Staged file artex.new exists → verify + smoke; pass → swap and request restart; fail → discard and keep running old version
//	② Only marker file remains     → just swapped, count an attempt; enough consecutive failures → rollback
//	③ Nothing exists               → normal startup
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] skipping bootstrap: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged handles the "staged file exists" scenario: verify then swap, or
// discard on failure.
//
// This is the only place in the entire upgrade chain that overwrites the
// executable, and it's the last gate — the smoke test catches download
// corruption, wrong architecture, missing dynamic libraries, etc. Once a
// non-runnable binary is let through, the watchdog script will tirelessly
// keep restarting it, and Go code never gets a chance to run, so automatic
// rollback is impossible.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] staged new version failed verification, discarded, continuing with current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "new version verification failed, discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] swap failed, continuing with current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "swap failed: " + err.Error()}
	}

	// Swap succeeded. Keep the marker for the next startup (which runs the new
	// version) to confirm stability.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to write upgrade marker (lost automatic rollback capability): %v", err)
	}
	log.Printf("[update] swapped to %s, exiting to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles "startup after swap": counts attempts, rolls back
// the old version if the limit is exceeded.
//
// The counter only increments after Go code starts running, so it covers
// "can execute but crashes during init" (incompatible config, port conflict,
// DB migration failure); "can't exec at all" is caught by the smoke test
// before the swap. Together they form the complete safety net.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// If rollback itself fails, don't restart again — it would loop
			// infinitely. Remove the marker and let the process start as-is;
			// if it can't start, at least the user can see the reason in logs.
			log.Printf("[update] new version failed to start %d consecutive times, and rollback failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "new version failed to start and rollback failed: " + err.Error()}
		}
		log.Printf("[update] new version failed to start %d consecutive times, rolled back to %s, exiting to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("new version failed to start, rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to update upgrade marker: %v", err)
	}
	log.Printf("[update] new version starting (attempt %d/%d), will confirm upgrade after stable run",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms the new version has been running stably, clearing the upgrade
// marker.
//
// Called by main after HTTP listening starts, with a delay: only surviving
// past this duration counts; otherwise the marker stays, and the next startup
// continues counting attempts until rollback is triggered.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // not a post-upgrade startup, nothing to do
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] failed to clear upgrade marker: %v", err)
		return
	}
	log.Printf("[update] new version running stably, upgrade complete (previous version kept as %s)", p.Old)
}

// SettleDelay is the runtime duration required to judge "the new version survived".
const SettleDelay = 30 * time.Second

// verifyStaged verifies the staged file: first compares SHA256, then actually
// launches it once.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("compute checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (download corrupted or tampered)")
	}
	return smokeTest(p.New)
}

// smokeTest launches the new binary with -h to confirm it can actually execute
// on the current system.
// This catches download truncation, wrong architecture (exec format error),
// missing dependencies, and a broad class of similar problems.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("set execute permission: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("smoke test timed out (new binary unresponsive)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap replaces the current binary with the staged new version.
//
// Both Unix and Windows allow renaming a running executable (Windows prohibits
// deleting and overwriting, but not renaming), so no platform-specific handling
// is needed, and we don't need to stop ourselves first.
func swap(p Paths) error {
	// Windows rename won't overwrite an existing target; the .old from a
	// previous upgrade must be cleared first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clean old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("backup current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// Swap failed but the current version was already moved away; it must
		// be put back as-is, otherwise there's no executable on next startup.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("install new version failed (%v), and restoring current version failed: %w", err, rerr)
		}
		return fmt.Errorf("install new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback swaps the old version backed up by swap back into place.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("no backup to roll back to %s: %w", p.Old, err)
	}
	// Move the non-working new version to .failed for investigation, rather
	// than deleting it outright.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("move failed version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restore old version: %w", err)
	}
	return nil
}

// Rollback implements /api/update/rollback: actively revert to the previous version.
// Only performs the swap; the restart is again left to the watchdog script
// (the caller subsequently exits with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("previous version cannot execute, refusing rollback: %w", err)
	}
	// Swap current and backup: after rollback you can still roll forward again.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("move current version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("install previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] failed to tidy backup after rollback (does not affect running): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether a rollback-capable previous version exists, so the
// frontend can decide whether to show the rollback button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown version"
	}
	return s
}
