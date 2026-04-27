// Copyright 2024 Dragonboat Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package integration contains end-to-end cluster tests.
//
// Tests spin up real Dragonboat NodeHost instances in-process, each backed by
// an independent miniredis instance (Redis db=1). No Docker or external
// services are required.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	dragonboat "github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/config"
	grpcserver "github.com/lni/dragonboat/v4/server/grpc"
	dbpb "github.com/lni/dragonboat/v4/server/proto"
	"github.com/lni/dragonboat/v4/server/statemachine"
)

const (
	testShardID  = 1
	rttMs        = 5  // very low RTT for in-process testing
	electionRTT  = 10 // election fires after 10×5ms = 50ms
	heartbeatRTT = 1  // heartbeat every 1×5ms = 5ms
)

// ─── testNode ────────────────────────────────────────────────────────────────

// testNode represents a single Dragonboat node in the integration cluster.
type testNode struct {
	id       uint64
	raftAddr string // stable across restarts
	dataDir  string // stable across restarts
	grpcAddr string // assigned once; new listener on restart

	redis   *miniredis.Miniredis
	nh      *dragonboat.NodeHost
	grpcSrv *grpc.Server
	conn    *grpc.ClientConn
	Client  dbpb.DragonboatServiceClient
}

// start initialises the NodeHost and gRPC server for this node.
// join=false + non-empty initialMembers → bootstrap.
// join=false + empty initialMembers    → restart from persisted data.
func (n *testNode) start(t *testing.T, join bool, initialMembers map[uint64]string) {
	t.Helper()

	nhCfg := config.NodeHostConfig{
		WALDir:         n.dataDir,
		NodeHostDir:    n.dataDir,
		RTTMillisecond: rttMs,
		RaftAddress:    n.raftAddr,
	}
	nh, err := dragonboat.NewNodeHost(nhCfg)
	if err != nil {
		t.Fatalf("node%d: NewNodeHost: %v", n.id, err)
	}
	n.nh = nh

	raftCfg := config.Config{
		ReplicaID:       n.id,
		ShardID:         testShardID,
		ElectionRTT:     electionRTT,
		HeartbeatRTT:    heartbeatRTT,
		CheckQuorum:     true,
		PreVote:         true,
		SnapshotEntries: 50, // snapshot frequently so node can recover via snapshot
		WaitReady:       true,
	}

	// Each node uses Redis db=1 on its own miniredis instance.
	factory := statemachine.NewRedisStateMachineFactory(statemachine.RedisConfig{
		Addr: n.redis.Addr(),
		DB:   1,
	})

	if err := nh.StartReplica(initialMembers, join, factory, raftCfg); err != nil {
		t.Fatalf("node%d: StartReplica: %v", n.id, err)
	}

	// gRPC server on a new free port (may differ from previous run).
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("node%d: listen: %v", n.id, err)
	}
	n.grpcAddr = lis.Addr().String()

	srv := grpc.NewServer()
	dbpb.RegisterDragonboatServiceServer(srv, grpcserver.New(nh, 5*time.Second))
	n.grpcSrv = srv
	go srv.Serve(lis) //nolint:errcheck

	conn, err := grpc.NewClient(n.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("node%d: dial: %v", n.id, err)
	}
	n.conn = conn
	n.Client = dbpb.NewDragonboatServiceClient(conn)
}

// stop shuts down the gRPC server and closes the NodeHost, simulating a crash
// or clean shutdown. The data directory and miniredis instance are preserved.
func (n *testNode) stop() {
	if n.conn != nil {
		n.conn.Close()
		n.conn = nil
		n.Client = nil
	}
	if n.grpcSrv != nil {
		n.grpcSrv.GracefulStop()
		n.grpcSrv = nil
	}
	if n.nh != nil {
		n.nh.Close()
		n.nh = nil
	}
}

// restart starts the node again using its persisted data directory.
// No initialMembers needed — Dragonboat reads bootstrap state from the log.
func (n *testNode) restart(t *testing.T) {
	t.Helper()
	n.stop()
	n.start(t, false, nil)
}

// redisGet reads a key directly from this node's miniredis DB 1 (bypassing Raft).
func (n *testNode) redisGet(key string) (string, bool) {
	prefix := fmt.Sprintf("db:%d:", testShardID)
	val, err := n.redis.DB(1).Get(prefix + key)
	if err != nil {
		return "", false
	}
	return val, true
}

// ─── testCluster ─────────────────────────────────────────────────────────────

// testCluster manages a set of testNodes.
type testCluster struct {
	t     *testing.T
	nodes []*testNode
}

// newCluster allocates ports and data dirs for n nodes but does NOT start them.
func newCluster(t *testing.T, size int) *testCluster {
	t.Helper()
	c := &testCluster{t: t}
	for i := 0; i < size; i++ {
		mr, err := miniredis.Run()
		if err != nil {
			t.Fatalf("miniredis: %v", err)
		}
		t.Cleanup(mr.Close)

		node := &testNode{
			id:       uint64(i + 1),
			raftAddr: freeAddr(t),
			dataDir:  t.TempDir(),
			redis:    mr,
		}
		c.nodes = append(c.nodes, node)
	}
	return c
}

// bootstrap starts all nodes simultaneously with the full initialMembers map.
func (c *testCluster) bootstrap() {
	t := c.t
	t.Helper()

	members := map[uint64]string{}
	for _, n := range c.nodes {
		members[n.id] = n.raftAddr
	}
	for _, n := range c.nodes {
		n.start(t, false, members)
	}
	t.Cleanup(func() {
		for _, n := range c.nodes {
			n.stop()
		}
	})
	c.waitForLeader()
}

// node returns the testNode for the given 1-based replica ID.
func (c *testCluster) node(id uint64) *testNode {
	return c.nodes[id-1]
}

// leader returns the node that is currently the Raft leader.
func (c *testCluster) leader() *testNode {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range c.nodes {
			if n.nh == nil {
				continue
			}
			leaderID, _, valid, err := n.nh.GetLeaderID(testShardID)
			if err == nil && valid && leaderID != 0 {
				return c.nodes[leaderID-1]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatal("timed out waiting for leader")
	return nil
}

// waitForLeader blocks until any live node reports a known leader.
func (c *testCluster) waitForLeader() {
	c.leader() // leader() already polls
}

// waitAllReady blocks until every live node reports a known leader,
// ensuring all nodes have joined the shard before tests proceed.
func (c *testCluster) waitAllReady() {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, n := range c.nodes {
			if n.nh == nil {
				continue
			}
			_, _, valid, err := n.nh.GetLeaderID(testShardID)
			if err != nil || !valid {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatal("timed out waiting for all nodes to be ready")
}

// propose writes a key-value pair through the cluster leader.
func (c *testCluster) propose(key, value string) {
	c.t.Helper()
	cmd, _ := json.Marshal(statemachine.KVCommand{Op: statemachine.OpSet, Key: key, Value: value})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leader := c.leader()
	_, err := leader.Client.Propose(ctx, &dbpb.ProposeRequest{
		ShardId: testShardID,
		Command: cmd,
	})
	if err != nil {
		c.t.Fatalf("Propose(%q=%q): %v", key, value, err)
	}
}

// readFrom performs a linearizable read on the specified node.
func (c *testCluster) readFrom(nodeID uint64, key string) statemachine.KVResult {
	c.t.Helper()
	query, _ := json.Marshal(statemachine.KVCommand{Op: statemachine.OpGet, Key: key})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.node(nodeID).Client.Read(ctx, &dbpb.ReadRequest{
		ShardId: testShardID,
		Query:   query,
	})
	if err != nil {
		c.t.Fatalf("Read on node%d(%q): %v", nodeID, key, err)
	}
	var result statemachine.KVResult
	json.Unmarshal(resp.Data, &result)
	return result
}

// staleReadFrom performs a stale (non-linearizable) read on the specified node.
// Used after a restart when the node may still be catching up.
func (c *testCluster) staleReadFrom(nodeID uint64, key string) statemachine.KVResult {
	c.t.Helper()
	query, _ := json.Marshal(statemachine.KVCommand{Op: statemachine.OpGet, Key: key})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := c.node(nodeID).Client.StaleRead(ctx, &dbpb.ReadRequest{
		ShardId: testShardID,
		Query:   query,
	})
	if err != nil {
		c.t.Fatalf("StaleRead on node%d(%q): %v", nodeID, key, err)
	}
	var result statemachine.KVResult
	json.Unmarshal(resp.Data, &result)
	return result
}

// waitForCatchup polls until the given node returns the expected value via
// stale read. This is used after restarting a node to confirm log replay is
// complete and the Redis state machine is up to date.
func (c *testCluster) waitForCatchup(nodeID uint64, key, expectedValue string) {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if r := c.staleReadFrom(nodeID, key); r.Found && r.Value == expectedValue {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("node%d did not catch up to key=%q value=%q within timeout", nodeID, key, expectedValue)
}

// ─── Integration Tests ────────────────────────────────────────────────────────

// TestCluster_NodeDropAndRestart is the main integration scenario:
//
//  1. Bootstrap a 3-node cluster, each backed by its own miniredis (db=1).
//  2. Write 10 keys through the leader.
//  3. Verify reads work on all 3 nodes.
//  4. Verify Redis on the leader has the keys.
//  5. DROP node3 (simulate crash / restart).
//  6. Verify the cluster still accepts writes with quorum = 2/3.
//  7. Write 5 more keys while node3 is down.
//  8. RESTART node3 from its persisted data directory.
//  9. Wait for node3 to catch up via Raft log replay.
//  10. Verify all 15 keys are readable via node3 (stale read).
//  11. Verify node3's Redis (db=1) reflects all 15 keys.
func TestCluster_NodeDropAndRestart(t *testing.T) {
	t.Log("=== Phase 1: Bootstrap 3-node cluster ===")

	c := newCluster(t, 3)
	c.bootstrap()
	c.waitAllReady()

	t.Logf("Cluster up. Leader: node%d", c.leader().id)

	// ── Phase 2: Write 10 keys ──────────────────────────────────────────────
	t.Log("=== Phase 2: Write 10 keys ===")

	initialKeys := make(map[string]string, 10)
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("key-%02d", i)
		v := fmt.Sprintf("val-%02d", i)
		initialKeys[k] = v
		c.propose(k, v)
	}
	t.Logf("Wrote %d keys", len(initialKeys))

	// ── Phase 3: Verify all nodes can read ─────────────────────────────────
	t.Log("=== Phase 3: Verify reads on all nodes ===")

	// Wait for each node to have applied all writes before asserting.
	for _, nodeID := range []uint64{1, 2, 3} {
		c.waitForCatchup(nodeID, "key-09", "val-09")
	}
	for _, nodeID := range []uint64{1, 2, 3} {
		for k, want := range initialKeys {
			got := c.staleReadFrom(nodeID, k)
			if !got.Found || got.Value != want {
				t.Errorf("node%d read(%q): found=%v value=%q, want %q", nodeID, k, got.Found, got.Value, want)
			}
		}
		t.Logf("node%d: all 10 keys readable ✓", nodeID)
	}

	// ── Phase 4: Verify Redis directly on node1 ──────────────────────────────
	t.Log("=== Phase 4: Verify Redis (db=1) on node1 ===")

	for k, want := range initialKeys {
		val, found := c.node(1).redisGet(k)
		if !found || val != want {
			t.Errorf("node1 Redis(%q): found=%v value=%q, want %q", k, found, val, want)
		}
	}
	t.Log("node1 Redis (db=1): all 10 keys present ✓")

	// ── Phase 5: Drop node3 ──────────────────────────────────────────────────
	t.Log("=== Phase 5: Drop node3 (simulate crash) ===")

	c.node(3).stop()
	t.Log("node3 stopped. Cluster quorum = 2/3, writes should continue.")

	// Allow leader election to settle if node3 was the leader.
	c.waitForLeader()
	t.Logf("Leader after node3 drop: node%d", c.leader().id)

	// ── Phase 6: Write 5 more keys with node3 down ──────────────────────────
	t.Log("=== Phase 6: Write 5 more keys (node3 is down) ===")

	laterKeys := make(map[string]string, 5)
	for i := 10; i < 15; i++ {
		k := fmt.Sprintf("key-%02d", i)
		v := fmt.Sprintf("val-%02d", i)
		laterKeys[k] = v
		c.propose(k, v)
	}
	t.Logf("Wrote %d more keys while node3 was down ✓", len(laterKeys))

	// Confirm node1 and node2 can see all 15 keys.
	allKeys := make(map[string]string, 15)
	for k, v := range initialKeys {
		allKeys[k] = v
	}
	for k, v := range laterKeys {
		allKeys[k] = v
	}
	for _, nodeID := range []uint64{1, 2} {
		for k, want := range allKeys {
			got := c.readFrom(nodeID, k)
			if !got.Found || got.Value != want {
				t.Errorf("node%d read(%q): found=%v value=%q, want %q", nodeID, k, got.Found, got.Value, want)
			}
		}
		t.Logf("node%d: all 15 keys readable ✓", nodeID)
	}

	// ── Phase 7: Restart node3 ───────────────────────────────────────────────
	t.Log("=== Phase 7: Restart node3 (from persisted data dir) ===")

	c.node(3).restart(t)
	t.Log("node3 restarted — waiting for it to catch up via log replay...")

	// ── Phase 8: Wait for node3 to catch up ─────────────────────────────────
	t.Log("=== Phase 8: Wait for node3 to catch up ===")

	// Use the last key written while node3 was down as the catchup sentinel.
	c.waitForCatchup(3, "key-14", "val-14")
	t.Log("node3 has caught up ✓")

	// ── Phase 9: Verify all 15 keys on node3 via stale read ─────────────────
	t.Log("=== Phase 9: Verify all 15 keys on node3 ===")

	for k, want := range allKeys {
		got := c.staleReadFrom(3, k)
		if !got.Found || got.Value != want {
			t.Errorf("node3 stale-read(%q): found=%v value=%q, want %q", k, got.Found, got.Value, want)
		}
	}
	t.Log("node3: all 15 keys readable via stale read ✓")

	// ── Phase 10: Verify node3's Redis (db=1) ───────────────────────────────
	t.Log("=== Phase 10: Verify Redis (db=1) on node3 ===")

	redisOK := 0
	for k, want := range allKeys {
		val, found := c.node(3).redisGet(k)
		if !found || val != want {
			t.Errorf("node3 Redis(%q): found=%v value=%q, want %q", k, found, val, want)
		} else {
			redisOK++
		}
	}
	if redisOK == 15 {
		t.Logf("node3 Redis (db=1): %d/15 keys present ✓", redisOK)
	} else {
		t.Logf("node3 Redis (db=1): %d/15 keys present", redisOK)
	}

	// ── Phase 11: Confirm full cluster health ────────────────────────────────
	t.Log("=== Phase 11: Full cluster health check ===")

	for _, n := range c.nodes {
		resp, err := n.Client.HealthCheck(context.Background(), &dbpb.HealthCheckRequest{})
		if err != nil || resp.Status != "OK" {
			t.Errorf("node%d health check failed: %v", n.id, err)
		} else {
			t.Logf("node%d: status=OK  nhid=%s ✓", n.id, resp.NodeHostId)
		}
	}

	t.Log("=== ALL PHASES PASSED ===")
}

// TestCluster_QuorumRequired verifies that when 2 of 3 nodes are down,
// the remaining node cannot make progress (no quorum).
func TestCluster_QuorumRequired(t *testing.T) {
	c := newCluster(t, 3)
	c.bootstrap()

	// Write one key to establish baseline.
	c.propose("quorum-test", "before")

	// Take down two nodes — only node1 remains.
	c.node(2).stop()
	c.node(3).stop()
	t.Log("Nodes 2 and 3 stopped — no quorum")

	// Try to propose with a tight timeout — must fail or time out.
	cmd, _ := json.Marshal(statemachine.KVCommand{Op: statemachine.OpSet, Key: "quorum-test", Value: "after"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.node(1).Client.Propose(ctx, &dbpb.ProposeRequest{
		ShardId: testShardID,
		Command: cmd,
	})
	if err == nil {
		t.Error("expected Propose to fail without quorum, but it succeeded")
	} else {
		t.Logf("Propose correctly failed without quorum: %v ✓", err)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// freeAddr returns a random free TCP address on localhost.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// init suppresses Dragonboat's verbose log output in tests.
func init() {
	os.Setenv("DRAGONBOAT_TEST_DISABLE_LOG_TO_STDERR", "1")
}
