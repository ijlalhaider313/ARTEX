#!/bin/sh
# ARTEX supervisor start script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run persistently in the background
#   ./start.sh -addr :9000           Extra args are passed through to artex verbatim
#
# It does one thing: run artex, and after the process exits decide from the exit
# code whether to relaunch it.
#
#   0      Normal user stop        → exit the loop
#   75     Program requested restart → relaunch immediately (in-app "one-click update" or "rollback")
#   other  Crash                  → back off and relaunch (1→2→4… up to 60s)
#
# Deliberately no download, SHA256 verification, or binary swap here: that logic would
# have to be written twice for sh and bat, and it is the one step that must never go wrong —
# once a broken binary is swapped in, this script would faithfully relaunch it over and over,
# leaving the user to rescue the machine by hand. So verification/swapping stays in Go
# (the selfupdate package), performed by artex itself at startup; this script stays dumb.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex process itself.
#
# Under Docker this is required: docker stop only sends SIGTERM to PID 1 (this script),
# not to child processes. Without forwarding, artex never receives the signal, cannot shut
# down gracefully, and after 10 seconds gets hard-killed by SIGKILL — leaving running tasks
# broken off mid-flight.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. At that point the child is
	# actually still shutting down gracefully; we must wait again to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is staged: after relaunch artex completes the swap at startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying new version)..."
			delay=1
			;;
		*)
			echo "[artex] abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
