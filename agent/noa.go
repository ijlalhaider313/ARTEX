package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compression" mechanism introduced in norma
// v0.4.0, available as a platform experimental feature toggled by the user in
// system settings. It is mutually exclusive with built-in compaction:
// noaadapter.Enable is the sole entry point; calling it once attaches the context
// takeover manager (Compactor), the Compress tool, and three persistent prompt
// segments; not calling Enable means it's off (built-in compaction works as
// usual). The toggle is resolved per run via the noaEnabledFn injected into each
// agent, read once per run, so switching only affects subsequently started runs —
// no need to rebuild the agent.

// enableNoa wires noa into opts when the resolver reports it's enabled.
// archiveRoot is the persistence base directory for compressed originals
// (taken from the global workDir; all agents store under <workDir>/noa, not
// scattered across task/intent directories); sessionID names the archive
// subdirectory beneath it (globally unique, so no collision within one base).
//
// noa is an experimental feature: a wiring failure must not interrupt real
// tasks. On error, report via onWarn and fall back to built-in compression.
// On success, clear opts.Compaction to avoid agentcore warning about
// "two context managers set simultaneously."
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa compression enable failed, falling back to built-in compression: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but when both are present agentcore warns every time; explicitly clear it.
	opts.Compaction = nil
}
