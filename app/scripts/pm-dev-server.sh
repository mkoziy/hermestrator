#!/bin/sh
# Keep the PM process tied to the Genkit Developer UI lifecycle. Genkit starts
# this command as a child; if it exits after Ctrl-C, the original parent PID
# disappears and this supervisor stops the dashboard instead of leaving :8080
# occupied by an orphaned process.
set -eu

parent_pid=$PPID
child_pid=""

cleanup() {
	if [ -z "$child_pid" ]; then
		return
	fi

	kill -TERM "$child_pid" 2>/dev/null || true
	(
		sleep 3
		kill -KILL "$child_pid" 2>/dev/null || true
	) &
	killer_pid=$!
	wait "$child_pid" 2>/dev/null || true
	kill "$killer_pid" 2>/dev/null || true
	wait "$killer_pid" 2>/dev/null || true
	child_pid=""
}

trap 'cleanup; exit 0' INT TERM HUP
trap cleanup EXIT

./.bin/pm-dev &
child_pid=$!

while kill -0 "$child_pid" 2>/dev/null; do
	if ! kill -0 "$parent_pid" 2>/dev/null; then
		exit 0
	fi
	sleep 1
done

wait "$child_pid"
