# Dragonboat Cluster Operations

## Overview

This server runs in two modes:

| Mode | Flag | Addresses nodes by | Use when |
|------|------|--------------------|----------|
| **Static** | *(default)* | `RaftAddress` (IP:port) | Fixed IPs, dev, single machine |
| **Gossip** | `--gossip` | Stable `NodeHostID` UUID | Cloud, containers, dynamic IPs |

In **gossip mode**, each node has a **stable UUID identity** (`NodeHostID`) that never changes, even if the node restarts on a different IP. The gossip ring propagates the current `RaftAddress → NodeHostID` mapping automatically. The cluster self-heals as long as **N/2 + 1** nodes are alive.

---

## Cluster Sizing

| Nodes | Quorum | Failure tolerance |
|-------|--------|-------------------|
| 1 | 1 | 0 — no fault tolerance |
| 3 | 2 | **1 node** can be down |
| 5 | 3 | **2 nodes** can be down |

For production, use **5 nodes** if you need to survive simultaneous failures (e.g. rolling restarts during a deployment that briefly takes 2 nodes offline).

---

## Ports Per Node

| Port | Protocol | Purpose |
|------|----------|---------|
| `63001` | TCP | Raft internal (node-to-node replication) |
| `63002` | TCP + UDP | Gossip (address discovery) |
| `8080` | TCP | gRPC API (client-facing) |
| `8081` | TCP | REST/JSON API (client-facing) |

Only `8080` / `8081` need to be exposed outside the cluster. `63001` and `63002` must be reachable between nodes but should be firewalled from external traffic.

---

## Bootstrap (First Time)

Bootstrap happens **once**. After this, nodes restart themselves from the persisted log.

### Step 1 — Assign stable NodeHostIDs

Generate one UUID per node. These never change:

```bash
node1_id=$(uuidgen)   # e.g. "a1b2c3d4-..."
node2_id=$(uuidgen)
node3_id=$(uuidgen)

echo "Node 1: $node1_id"
echo "Node 2: $node2_id"
echo "Node 3: $node3_id"
```

In Kubernetes, use a ConfigMap or Secret to store and inject these.

### Step 2 — Start all nodes simultaneously with `--initial-members`

Every node in the cluster must receive the **same** `--initial-members` map.

```bash
# On node 1 (IP: 10.0.0.1)
dragonboat-server \
  --gossip \
  --node-id=1 --shard-id=1 \
  --raft-addr=10.0.0.1:63001 \
  --gossip-addr=0.0.0.0:63002 \
  --gossip-advertise=10.0.0.1:63002 \
  --gossip-seeds=10.0.0.2:63002,10.0.0.3:63002 \
  --data-dir=/data \
  --initial-members=1=$node1_id,2=$node2_id,3=$node3_id \
  --node-host-id=$node1_id

# On node 2 (IP: 10.0.0.2)
dragonboat-server \
  --gossip \
  --node-id=2 --shard-id=1 \
  --raft-addr=10.0.0.2:63001 \
  --gossip-addr=0.0.0.0:63002 \
  --gossip-advertise=10.0.0.2:63002 \
  --gossip-seeds=10.0.0.1:63002,10.0.0.3:63002 \
  --data-dir=/data \
  --initial-members=1=$node1_id,2=$node2_id,3=$node3_id \
  --node-host-id=$node2_id

# On node 3 (IP: 10.0.0.3) — same pattern
```

> **Tip:** Nodes do not have to start simultaneously. They will wait for a majority to form before electing a leader. Starting within ~30 seconds of each other is fine.

---

## Normal Restart (After Bootstrap)

**Do not pass `--initial-members` on restart.** Dragonboat reads its bootstrap config from the data directory.

```bash
# Node 1 comes back on a NEW IP (10.1.2.3 instead of 10.0.0.1):
dragonboat-server \
  --gossip \
  --node-id=1 --shard-id=1 \
  --raft-addr=10.1.2.3:63001 \
  --gossip-addr=0.0.0.0:63002 \
  --gossip-advertise=10.1.2.3:63002 \
  --gossip-seeds=10.0.0.2:63002 \
  --data-dir=/data
  # --initial-members is OMITTED on restart
```

Gossip broadcasts the new IP to all other nodes. Dragonboat reconnects automatically within one election timeout.

---

## Adding a New Node (Scale Out)

To grow the cluster from 3 → 4 nodes:

```bash
# Step 1: Start the new node with --join
node4_id=$(uuidgen)
dragonboat-server \
  --gossip \
  --node-id=4 --shard-id=1 \
  --raft-addr=10.0.0.4:63001 \
  --gossip-addr=0.0.0.0:63002 \
  --gossip-advertise=10.0.0.4:63002 \
  --gossip-seeds=10.0.0.1:63002 \
  --data-dir=/data \
  --node-host-id=$node4_id \
  --join

# Step 2: Register the new replica with the leader via REST
curl -X POST http://10.0.0.1:8081/v1/shards/1/replicas \
     -H 'Content-Type: application/json' \
     -d "{\"replica_id\": 4, \"target\": \"$node4_id\"}"
```

The leader adds the new node to its membership log. The new node bootstraps via a snapshot + log replay.

---

## Removing a Node (Scale In)

```bash
# Remove replica 3 from shard 1 (call this on any live node)
curl -X DELETE http://10.0.0.1:8081/v1/shards/1/replicas/3
```

Then stop the node process. The data directory can be wiped.

> **Warning:** Never remove nodes below quorum. On a 3-node cluster, you must expand to 4 nodes before removing one, if you want to maintain 1-failure tolerance throughout.

---

## Health Checks

```bash
# Is this node alive?
curl http://10.0.0.1:8081/healthz
# → {"status":"OK","nodeHostId":"..."}

# Who is the leader?
curl http://10.0.0.1:8081/v1/shards/1/leader
# → {"leaderId":"2","term":"5","available":true}

# What is the current membership?
curl http://10.0.0.1:8081/v1/shards/1/membership

# Full NodeHost status
curl http://10.0.0.1:8081/v1/nodehost
```

---

## Docker Compose (Local 3-Node Cluster)

```bash
# Start all three nodes
docker compose -f deployments/docker-compose.yml up --build

# REST API is on node1 at port 8081
curl http://localhost:8081/healthz

# Kill node3 and verify writes still work (quorum = 2/3)
docker compose -f deployments/docker-compose.yml stop node3
curl -X POST http://localhost:8081/v1/shards/1/propose \
     -d '{"command":"eyJvcCI6InNldCIsImtleSI6ImZvbyIsInZhbHVlIjoiYmFyIn0="}'

# Restart node3 — it auto-rejoins
docker compose -f deployments/docker-compose.yml start node3

# Add node4 (scale-out profile)
docker compose -f deployments/docker-compose.yml \
  --profile scale-out up node4 -d
# Then call AddReplica REST endpoint (see above)
```

---

## Kubernetes

In Kubernetes, use a `StatefulSet` + headless `Service`. Each pod gets a stable DNS name (e.g. `dragonboat-0.dragonboat.default.svc.cluster.local`) which acts as a stable seed address even when the pod IP changes.

```yaml
# Sketch — fill in your own values
env:
  - name: DRAGONBOAT_GOSSIP
    value: "true"
  - name: DRAGONBOAT_NODE_HOST_ID
    valueFrom:
      secretKeyRef:
        name: dragonboat-node-ids
        key: node-1-id        # stable UUID per pod ordinal
  - name: DRAGONBOAT_GOSSIP_SEEDS
    value: "dragonboat-0.dragonboat:63002,dragonboat-1.dragonboat:63002,dragonboat-2.dragonboat:63002"
  - name: DRAGONBOAT_GOSSIP_ADVERTISE
    valueFrom:
      fieldRef:
        fieldPath: status.podIP   # concrete IP for advertise
```

> **Note:** `AdvertiseAddress` **must** be a concrete IP (not a hostname). Use `status.podIP` for the gossip advertise address and the DNS name for the seed list.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Node can't join after restart | `--initial-members` passed on restart | Remove `--initial-members` flag on restart |
| Cluster stuck, no leader | All nodes have wrong gossip seeds | Ensure at least one seed is reachable |
| `AdvertiseAddress` error | Hostname used instead of IP | Use a concrete IP for `--gossip-advertise` |
| Write timeout after node failure | Leader election in progress | Wait one election cycle (~1–2 s at default settings), then retry |
| Node rejoins but data is stale | Expected — Dragonboat replays from snapshot | Wait for log catchup (logged as `applied ADD`) |
