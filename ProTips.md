# Caduceus Pro-Tips & Best Practices

When building distributed systems with Caduceus (Multi-Group Raft), mastering the lower-level primitives can dramatically improve your cluster's resilience and performance. Here are some pro-tips for operating Caduceus in production:

## 1. Use "Observers" for Safe Node Addition
When adding a brand new node to a large, existing Raft shard, the leader must stream a massive snapshot of the state machine to the new node. If you add the node as a full voting member immediately, the leader might wait on it for quorum, severely degrading cluster performance during the sync. 
**Pro-Tip:** Always add new nodes using `SyncRequestAddObserver`. Observers receive the snapshot and log streams but do not vote. Once the observer's log is fully caught up with the leader, promote it to a voting member.

## 2. Leverage `ReadIndex` for Fast, Consistent Reads
You don't need to write to the Raft log just to read data consistently. 
**Pro-Tip:** Use `SyncRead` to perform a linearizable read. This uses the Raft `ReadIndex` protocol: the leader confirms it still holds leadership by exchanging heartbeats with a quorum, and then serves the read from its local state machine. This bypasses expensive disk fsyncs entirely.

## 3. Keep State Machine Updates Pure and Deterministic
Your state machine's `Update()` method is invoked by Caduceus when a Raft log entry is committed. During crash recoveries or snapshot restorations, Caduceus might replay the Raft log and call `Update()` again for past entries.
**Pro-Tip:** Never put non-deterministic logic (like generating random UUIDs or fetching the current time `time.Now()`) inside the `Update()` method. Never make external HTTP calls or trigger side-effects from within `Update()`. Generate UUIDs and timestamps *before* proposing the write, and store them in the payload.

## 4. Optimize Snapshot Tuning
A large Raft log takes up disk space and increases node startup time because the system has to replay every entry.
**Pro-Tip:** Tune `CompactionOverhead` and `SnapshotEntries` in your `Config`. If your state machine is small in memory but receives millions of updates, take snapshots frequently. If your state machine is massive (e.g., hundreds of GBs), take snapshots less frequently to avoid saturating disk I/O, but ensure you have enough disk space for the growing Raft log.

## 5. Enable and Understand Pre-Vote
By default, standard Raft suffers from "disruptive servers"—if a follower gets network partitioned, it constantly calls for elections, driving up its Term. When the partition heals, its massive Term forces the healthy leader to step down.
**Pro-Tip:** Ensure `PreVote` is enabled in your `Config` (it usually is by default). Pre-vote requires a node to successfully poll a quorum *before* incrementing its Term, completely neutralizing the disruptive server problem.

## 6. Pebble vs. RocksDB Log Storage
Caduceus uses `Pebble` as its default LogDB engine. 
**Pro-Tip:** Stick with Pebble unless you have a highly specific reason not to. Pebble is written in pure Go, meaning you don't have to deal with CGO cross-compilation headaches, and it prevents Go's garbage collector from fighting with C++ memory allocators. 

## 7. Catastrophic Quorum Loss Recovery
If a 3-node shard loses 2 nodes simultaneously (e.g., a data center burns down), you have lost quorum. Standard Raft cannot recover from this because the remaining node cannot elect itself or commit a ConfigChange to remove the dead nodes.
**Pro-Tip:** Caduceus provides offline recovery tools. You can use the snapshot and the surviving Raft log to manually reconstruct the state machine, or use the built-in `Repair` utility to forcibly rewrite the Raft configuration and resurrect the shard from the surviving node.
