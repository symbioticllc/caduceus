#!/bin/bash
set -e

if [ "$#" -ne 5 ]; then
    echo "Usage: $0 <api_endpoint> <shard_id> <dead_replica_id> <new_replica_id> <new_target_addr>"
    echo "Example: $0 http://localhost:8081 1 2 4 localhost:63004"
    echo ""
    echo "This script automates the recovery of a shard by removing a dead node"
    echo "and adding a newly provisioned node to replace it."
    exit 1
fi

API=$1
SHARD=$2
DEAD_ID=$3
NEW_ID=$4
TARGET=$5

echo "=========================================================="
echo " Starting Node Replacement Workflow"
echo "=========================================================="
echo "Target Cluster API : ${API}"
echo "Raft Shard         : ${SHARD}"
echo "Dead Replica ID    : ${DEAD_ID}"
echo "New Replica ID     : ${NEW_ID}"
echo "New Replica Addr   : ${TARGET}"
echo "=========================================================="

echo -e "\n[1/2] Removing dead replica ${DEAD_ID} from shard ${SHARD}..."
curl -sS -X DELETE "${API}/v1/shards/${SHARD}/replicas/${DEAD_ID}" \
     -H "Content-Type: application/json"
echo " ✓ Node removed successfully."

echo -e "\n[2/2] Adding new replica ${NEW_ID} to shard ${SHARD}..."
curl -sS -X POST "${API}/v1/shards/${SHARD}/replicas" \
     -H "Content-Type: application/json" \
     -d '{
       "shard_id": '"${SHARD}"',
       "replica_id": '"${NEW_ID}"',
       "target": "'"${TARGET}"'",
       "timeout_ms": 5000
     }'
echo " ✓ Node added successfully."

echo -e "\n=========================================================="
echo " Replacement initiated! The cluster leader will now stream"
echo " a snapshot and Raft logs to the new node to synchronize it."
echo "=========================================================="
