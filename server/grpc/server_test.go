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

package grpcserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	dragonboat "github.com/symbioticllc/caduceus/v4"
	"github.com/symbioticllc/caduceus/v4/config"
	grpcserver "github.com/symbioticllc/caduceus/v4/server/grpc"
	dbpb "github.com/symbioticllc/caduceus/v4/server/proto"
	"github.com/symbioticllc/caduceus/v4/server/statemachine"
)

// spinUpNode starts a single-node Dragonboat cluster and a gRPC server.
// It returns the gRPC client, a cleanup func, and any startup error.
func spinUpNode(t *testing.T) (dbpb.DragonboatServiceClient, func()) {
	t.Helper()

	dataDir := filepath.Join(os.TempDir(), fmt.Sprintf("dragonboat-test-%d", time.Now().UnixNano()))
	raftAddr := freeAddr(t)

	nhCfg := config.NodeHostConfig{
		WALDir:         dataDir,
		NodeHostDir:    dataDir,
		RTTMillisecond: 5,
		RaftAddress:    raftAddr,
	}
	nh, err := dragonboat.NewNodeHost(nhCfg)
	if err != nil {
		t.Fatalf("NewNodeHost: %v", err)
	}

	raftCfg := config.Config{
		ReplicaID:       1,
		ShardID:         1,
		ElectionRTT:     10,
		HeartbeatRTT:    1,
		CheckQuorum:     true,
		SnapshotEntries: 0,
		WaitReady:       true,
	}
	initialMembers := map[uint64]string{1: raftAddr}
	if err := nh.StartReplica(initialMembers, false, statemachine.NewKVStateMachine, raftCfg); err != nil {
		nh.Close()
		t.Fatalf("StartReplica: %v", err)
	}

	// Wait for leader election.
	waitForLeader(t, nh, 1)

	// Start the gRPC server on a random port.
	grpcAddr := freeAddr(t)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		nh.Close()
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	dbpb.RegisterDragonboatServiceServer(srv, grpcserver.New(nh, 5*time.Second))
	go srv.Serve(lis) //nolint:errcheck

	// Dial the client.
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		nh.Close()
		t.Fatalf("dial: %v", err)
	}

	client := dbpb.NewDragonboatServiceClient(conn)
	cleanup := func() {
		conn.Close()
		srv.GracefulStop()
		nh.Close()
		os.RemoveAll(dataDir)
	}
	return client, cleanup
}

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestHealthCheck(t *testing.T) {
	client, cleanup := spinUpNode(t)
	defer cleanup()

	resp, err := client.HealthCheck(context.Background(), &dbpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if resp.Status != "OK" {
		t.Errorf("status = %q, want OK", resp.Status)
	}
	if resp.NodeHostId == "" {
		t.Errorf("NodeHostId is empty")
	}
}

func TestProposeAndRead(t *testing.T) {
	client, cleanup := spinUpNode(t)
	defer cleanup()

	cmd := mustMarshal(t, statemachine.KVCommand{Op: statemachine.OpSet, Key: "hello", Value: "world"})

	// Propose.
	presp, err := client.Propose(context.Background(), &dbpb.ProposeRequest{
		ShardId: 1,
		Command: cmd,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if presp.Value != 1 {
		t.Errorf("propose result.Value = %d, want 1", presp.Value)
	}

	// Linearisable read.
	query := mustMarshal(t, statemachine.KVCommand{Op: statemachine.OpGet, Key: "hello"})
	rresp, err := client.Read(context.Background(), &dbpb.ReadRequest{
		ShardId: 1,
		Query:   query,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var result statemachine.KVResult
	if err := json.Unmarshal(rresp.Data, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !result.Found {
		t.Errorf("key 'hello' not found after propose")
	}
	if result.Value != "world" {
		t.Errorf("value = %q, want 'world'", result.Value)
	}
}

func TestStaleRead(t *testing.T) {
	client, cleanup := spinUpNode(t)
	defer cleanup()

	// Set a key first.
	cmd := mustMarshal(t, statemachine.KVCommand{Op: statemachine.OpSet, Key: "stale", Value: "yes"})
	if _, err := client.Propose(context.Background(), &dbpb.ProposeRequest{ShardId: 1, Command: cmd}); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	query := mustMarshal(t, statemachine.KVCommand{Op: statemachine.OpGet, Key: "stale"})
	resp, err := client.StaleRead(context.Background(), &dbpb.ReadRequest{ShardId: 1, Query: query})
	if err != nil {
		t.Fatalf("StaleRead: %v", err)
	}
	var result statemachine.KVResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.Found || result.Value != "yes" {
		t.Errorf("stale read: found=%v value=%q", result.Found, result.Value)
	}
}

func TestGetLeader(t *testing.T) {
	client, cleanup := spinUpNode(t)
	defer cleanup()

	resp, err := client.GetLeader(context.Background(), &dbpb.GetLeaderRequest{ShardId: 1})
	if err != nil {
		t.Fatalf("GetLeader: %v", err)
	}
	if !resp.Available {
		t.Errorf("leader not available")
	}
	if resp.LeaderId == 0 {
		t.Errorf("leader id is 0")
	}
}

func TestGetMembership(t *testing.T) {
	client, cleanup := spinUpNode(t)
	defer cleanup()

	resp, err := client.GetMembership(context.Background(), &dbpb.GetMembershipRequest{ShardId: 1})
	if err != nil {
		t.Fatalf("GetMembership: %v", err)
	}
	if len(resp.Nodes) == 0 {
		t.Errorf("no nodes returned")
	}
}

func TestGetNodeHostInfo(t *testing.T) {
	client, cleanup := spinUpNode(t)
	defer cleanup()

	resp, err := client.GetNodeHostInfo(context.Background(), &dbpb.NodeHostInfoRequest{SkipLogInfo: true})
	if err != nil {
		t.Fatalf("GetNodeHostInfo: %v", err)
	}
	if resp.NodeHostId == "" {
		t.Errorf("empty NodeHostId")
	}
}

func TestDeleteKey(t *testing.T) {
	client, cleanup := spinUpNode(t)
	defer cleanup()

	// Set then delete.
	set := mustMarshal(t, statemachine.KVCommand{Op: statemachine.OpSet, Key: "gone", Value: "soon"})
	if _, err := client.Propose(context.Background(), &dbpb.ProposeRequest{ShardId: 1, Command: set}); err != nil {
		t.Fatalf("Propose set: %v", err)
	}
	del := mustMarshal(t, statemachine.KVCommand{Op: statemachine.OpDelete, Key: "gone"})
	resp, err := client.Propose(context.Background(), &dbpb.ProposeRequest{ShardId: 1, Command: del})
	if err != nil {
		t.Fatalf("Propose delete: %v", err)
	}
	if resp.Value != 1 {
		t.Errorf("delete result.Value = %d, want 1 (key existed)", resp.Value)
	}

	// Confirm it's gone.
	query := mustMarshal(t, statemachine.KVCommand{Op: statemachine.OpGet, Key: "gone"})
	rresp, _ := client.Read(context.Background(), &dbpb.ReadRequest{ShardId: 1, Query: query})
	var result statemachine.KVResult
	json.Unmarshal(rresp.Data, &result)
	if result.Found {
		t.Errorf("key 'gone' still exists after delete")
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

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

// waitForLeader polls until the specified shard has a known leader or the
// test times out.
func waitForLeader(t *testing.T, nh *dragonboat.NodeHost, shardID uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, _, ok, err := nh.GetLeaderID(shardID)
		if err == nil && ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for leader on shard %d", shardID)
}
