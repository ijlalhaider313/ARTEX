// Package selfupdate implements ARTEX's one-click web update: fetches a new
// binary from a GitHub Release, verifies it, stages it, and atomically swaps
// it in on the next startup.
//
// Overall division of labor (see start.sh / start.bat):
//
//	Startup script = a simple watchdog loop, only responsible for
//	"restarting the process based on exit code after it exits"
//	This package    = all the error-prone logic (download / SHA256 verify /
//	smoke test / swap / rollback on failure)
//
// The swap logic is in Go rather than in the script because SHA256 verification
// and smoke testing would require two implementations across sh and bat
// (sha256sum / shasum / certutil), and this is the one step that absolutely must
// not get wrong — if you swap in a binary that can't run, the watchdog will
// faithfully keep restarting it, and the user has to log in and fix it manually.
//
// A complete upgrade goes through three process starts:
//
//	① Old server receives /api/update/apply → download+verify → stage artex.new → exit 75
//	② Script restarts old version → Bootstrap finds artex.new → verify+smoke → swap → exit 75
//	③ Script restarts, now running new version → Bootstrap records an attempt → clears marker after successful startup
//
// Any step failing falls back to the old version: ② if verification fails, delete the staged file and keep running old version; ③ if it crashes 3 consecutive times before
// clearing the marker (can't start at all) it automatically swaps artex.old back.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the "please restart me" exit code (EX_TEMPFAIL). The startup
// script immediately re-runs on seeing it, without counting it toward crash
// backoff. 0 means the user stopped normally (script exits the loop); all other
// codes are treated as crashes.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after a swap. The new
// version increments the counter on each start; surviving past settleDelay
// clears the marker; crashing maxAttempts times means the new version can't
// run at all, triggering automatic rollback.
const maxAttempts = 3

// Paths are all the files involved in an upgrade, all rooted in **the executable's
// directory**. CWD is deliberately avoided: when running as a service the working
// directory might be / or any path, and using CWD would leave staged files
// elsewhere, breaking the swap logic entirely.
type Paths struct {
	Dir     string // executable's directory
	Current string // currently running binary       artex      / artex.exe
	New     string // staged new version             artex.new  / artex.new.exe
	Sum     string // new version's sha256 (hex)     artex.new.sha256 / artex.new.exe.sha256
	Old     string // backup of old version pre-swap artex.old  / artex.old.exe
	Marker  string // upgrade state marker           artex.upgrade.json
}

// ResolvePaths derives all upgrade paths from the current executable.
//
// On Windows .new/.old must also carry the .exe suffix, otherwise smoke testing
// and post-swap execution will fail, so the suffix is stripped first and
// re-appended, keeping the naming symmetric across both platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records the progress of a swap, used to trigger automatic rollback
// when the new version can't start.
type marker struct {
	From     string `json:"from"`     // version before upgrade
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // startup attempts since swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes staged files. Called on swap success, verification
// failure, or user cancellation, to prevent a leftover artex.new from being
// retried on the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers, returning -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version number (e.g. a
// local dev build's "dev" or a git describe output like
// "0.3.7-2-gabc1234-dirty"); in that case the caller should disable one-click
// update, otherwise a dev build would get "upgraded" to a release version,
// overwriting uncommitted changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a "v0.3.7" / "0.3.7" version string into [3]int.
//
// Only accepts a clean three-segment form: build.sh uses git describe for non-tag
// builds, producing versions like "0.3.7-2-gabc1234" with suffixes — these must
// be judged as non-comparable rather than treated as 0.3.7, otherwise dev builds
// would be misjudged as "already latest" or overwritten by a release version.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process is running inside a container. Under
// Docker the swap writes to the container's writable layer; `docker compose up -d`
// recreating the container reverts to the image's bundled version — this is
// expected behavior (the user is pulling a new image at that point), but the
// frontend needs to be able to explain it accordingly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
