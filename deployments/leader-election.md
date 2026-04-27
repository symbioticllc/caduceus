# Leader Election

In Raft, the **leader** (analogous to a "master") is determined automatically by the protocol. It is never manually configured or assigned.

---

## Normal Operation

While a leader is alive it sends periodic **heartbeats** to all follower nodes. Each follower has a randomized **election timeout** — receiving a heartbeat resets that timer.

```
Leader  ──heartbeat──►  Follower 1  (timer reset)
        ──heartbeat──►  Follower 2  (timer reset)
        ──heartbeat──►  Follower 3  (timer reset)
```

The relevant config knobs (in `config.Config`):

| Field | Meaning |
|-------|---------|
| `RTTMillisecond` | Estimated average round-trip time between nodes (ms) |
| `HeartbeatRTT` | Heartbeat interval = `HeartbeatRTT × RTTMillisecond` |
| `ElectionRTT` | Election timeout = `ElectionRTT × RTTMillisecond` (randomised between 1× and 2×) |

Example: with `RTTMillisecond=200`, `HeartbeatRTT=1`, `ElectionRTT=10`:
- Heartbeats fire every **200 ms**
- Election triggers after **2–4 s** of silence

---

## What Happens When a Leader Dies

1. A follower's election timer **expires** without receiving a heartbeat.
2. That follower becomes a **candidate**, increments its *term* (a monotonically increasing logical clock), and votes for itself.
3. It broadcasts a `RequestVote` RPC to all peers.
4. A peer grants its vote if:
   - It hasn't already voted in this term, **and**
   - The candidate's log is at least as up-to-date as its own.
5. The **first candidate to receive N/2 + 1 votes** wins and declares itself leader.
6. The new leader immediately starts sending heartbeats to suppress further elections.

```
Node 3 dies   →   Node 1 timer expires first (randomised)
              →   Node 1 requests votes from Node 2 and Node 4
              →   Both grant votes (no competing candidate yet)
              →   Node 1 becomes leader, begins heartbeats
```

### Why timers are randomised

Without randomisation every node would time out simultaneously, all vote for themselves, and no majority would form (a *split vote*). Randomisation ensures one node almost always fires first.

---

## PreVote (Enabled by Default)

Caduceus sets `PreVote: true` in the Raft config. Before starting a real election, a candidate first asks:

> "Would you vote for me *if* I started an election?"

A real election only begins if the candidate wins a majority of pre-votes. This prevents a **network-partitioned node** from repeatedly bumping the term number and causing unnecessary disruption when it rejoins.

---

## Querying the Current Leader

```bash
# REST
curl http://localhost:8081/v1/shards/1/leader
# {"leaderId":"2","term":"7","available":true}

# gRPC (grpcurl)
grpcurl -plaintext localhost:8080 \
  dragonboat.v1.DragonboatService/GetLeader \
  <<< '{"shard_id": 1}'
```

The `leaderId` is the **replica ID** (1, 2, 3, …). It can change any time a node restarts or a network partition heals.

---

## Client Request Routing

Clients **do not need to know which node is the leader**. Every node in the cluster accepts gRPC / REST calls. Dragonboat internally forwards proposals to the current leader via its transport layer. If the leader changes mid-request, the call returns a timeout and the client retries.

```
Client  ──POST /v1/shards/1/propose──►  Node 2 (follower)
                                         │
                                         │  internally forwarded
                                         ▼
                                       Node 1 (leader)  ──committed──►  all nodes
```

---

## Key Properties

| Property | Detail |
|----------|--------|
| Automatic | Never manually assigned; protocol-elected |
| Unique | At most one leader per term, globally |
| Any node can win | First with an up-to-date log and enough votes |
| Failure detection | Based on heartbeat timeouts, not TCP liveness |
| Transparent to clients | Route requests to any node; library handles the rest |

---

## Related Configuration

```go
raftCfg := config.Config{
    ReplicaID:    1,
    ShardID:      1,
    ElectionRTT:  10,   // election timeout = 10 × RTTMillisecond
    HeartbeatRTT: 1,    // heartbeat = 1 × RTTMillisecond
    CheckQuorum:  true, // leader steps down if it can't reach quorum
    PreVote:      true, // prevents term inflation from partitioned nodes
}
```

`CheckQuorum: true` causes the leader to **voluntarily step down** if it stops hearing from a majority of followers — ensuring a new leader can be elected even when the old one is isolated in a network partition.

---

## See Also

- [Operational Runbook](./README.md) — bootstrap, restart, scale-out
- [`config.Config` godoc](https://pkg.go.dev/github.com/symbioticllc/caduceus/v4/config#Config)
- [Raft thesis §3](https://raft.github.io/raft.pdf) — the authoritative source
