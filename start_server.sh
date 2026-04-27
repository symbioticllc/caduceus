#!/bin/bash
set -e

# Data directory for the node
DATA_DIR="/tmp/db1"

# Ensure the data directory exists
mkdir -p "${DATA_DIR}"

echo "Starting Caduceus NodeHost (Node 1, Shard 1)..."
echo "Backend: Redis"
echo "Data Directory: ${DATA_DIR}"

go run cmd/dragonboat-server/main.go \
  --node-id=1 \
  --shard-id=1 \
  --raft-addr=localhost:63001 \
  --grpc-addr=:8080 \
  --rest-addr=:8081 \
  --data-dir="${DATA_DIR}" \
  --initial-members=1=localhost:63001 \
  --backend=redis \
  --redis-addr=localhost:6379
