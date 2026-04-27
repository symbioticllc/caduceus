#!/bin/bash
set -e

echo "Looking for dragonboat-server process..."

# Find the PID of the running server
PID=$(pgrep -f "cmd/dragonboat-server/main.go" || true)

if [ -z "$PID" ]; then
    echo "No dragonboat-server process found running."
    exit 0
fi

echo "Found dragonboat-server running with PID(s):"
echo "$PID"
echo "Sending SIGTERM to gracefully shut down..."

# Send the termination signal
kill -TERM $PID

echo "Waiting for processes to cleanly terminate and flush to disk..."

# Wait until the processes no longer exist
for p in $PID; do
    while kill -0 "$p" 2>/dev/null; do
        sleep 0.5
    done
done

echo "Server cleanly shut down!"
