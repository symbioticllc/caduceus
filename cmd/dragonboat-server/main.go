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

// Package main provides the dragonboat-server binary — a self-contained
// process that runs a single Dragonboat NodeHost backed by the bundled
// in-memory KV state machine and exposes it over gRPC and REST/JSON.
//
// # Operating modes
//
// Static mode (default): nodes are addressed by their static RaftAddress.
// Good for dev and fixed-IP environments.
//
// Gossip mode (--gossip): nodes are addressed by a stable NodeHostID UUID
// and advertise their current RaftAddress via a gossip ring. Nodes can
// restart on different IPs — the cluster reconnects automatically.
//
// # Static quick-start (single node)
//
//	dragonboat-server \
//	  --node-id=1 --shard-id=1 \
//	  --raft-addr=localhost:63001 \
//	  --grpc-addr=:8080 --rest-addr=:8081 \
//	  --data-dir=/tmp/db1 \
//	  --initial-members=1=localhost:63001
//
// # Gossip quick-start (bootstrap a 3-node cluster)
//
// On each node, set DRAGONBOAT_NODE_HOST_ID to a stable UUID and run:
//
//	dragonboat-server \
//	  --gossip \
//	  --node-id=1 --shard-id=1 \
//	  --raft-addr=10.0.0.1:63001 \
//	  --gossip-addr=0.0.0.0:63002 \
//	  --gossip-advertise=10.0.0.1:63002 \
//	  --gossip-seeds=10.0.0.2:63002,10.0.0.3:63002 \
//	  --grpc-addr=:8080 --rest-addr=:8081 \
//	  --data-dir=/data \
//	  --initial-members=1=<nhid-1>,2=<nhid-2>,3=<nhid-3>
//
// After first bootstrap, subsequent restarts need only:
//
//	dragonboat-server \
//	  --gossip \
//	  --node-id=1 --shard-id=1 \
//	  --raft-addr=<current-ip>:63001 \
//	  --gossip-addr=0.0.0.0:63002 \
//	  --gossip-advertise=<current-ip>:63002 \
//	  --gossip-seeds=<any-live-node>:63002 \
//	  --data-dir=/data
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	dragonboat "github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/server/gateway"
	grpcserver "github.com/lni/dragonboat/v4/server/grpc"
	dbpb "github.com/lni/dragonboat/v4/server/proto"
	"github.com/lni/dragonboat/v4/server/statemachine"
)

func main() {
	// ── Core flags ──────────────────────────────────────────────────────────
	nodeID   := flag.Uint64("node-id", 1, "Replica ID of this node within the shard")
	shardID  := flag.Uint64("shard-id", 1, "Raft shard (group) ID")
	raftAddr := flag.String("raft-addr", "localhost:63001", "Raft internal address (host:port)")
	grpcAddr := flag.String("grpc-addr", ":8080", "gRPC listen address")
	restAddr := flag.String("rest-addr", ":8081", "REST/JSON gateway listen address")
	dataDir  := flag.String("data-dir", "/tmp/dragonboat-node", "Data directory for Raft logs and snapshots")
	members  := flag.String("initial-members", "",
		"Bootstrap membership. Static mode: id=raftAddr pairs. "+
			"Gossip mode: id=NodeHostID pairs. Omit on restart (data-dir has it).")
	joinCluster := flag.Bool("join", false, "Join an existing shard as a new member (no initial-members needed)")
	opTimeout   := flag.Duration("timeout", 5*time.Second, "Default per-operation timeout")

	// ── Gossip flags ─────────────────────────────────────────────────────────
	useGossip     := flag.Bool("gossip", envBool("DRAGONBOAT_GOSSIP"), "Enable dynamic gossip-based node registry")
	gossipAddr    := flag.String("gossip-addr", envOr("DRAGONBOAT_GOSSIP_ADDR", "0.0.0.0:63002"),
		"Local address the gossip agent binds to (host:port). Both UDP and TCP.")
	gossipAdvert  := flag.String("gossip-advertise", os.Getenv("DRAGONBOAT_GOSSIP_ADVERTISE"),
		"Publicly reachable IP:port for this node's gossip agent (must be a concrete IP).")
	gossipSeeds   := flag.String("gossip-seeds", os.Getenv("DRAGONBOAT_GOSSIP_SEEDS"),
		"Comma-separated list of seed gossip addresses (other nodes' --gossip-advertise values).")
	nodeHostID    := flag.String("node-host-id", os.Getenv("DRAGONBOAT_NODE_HOST_ID"),
		"Stable NodeHostID UUID for this node. Auto-generated and persisted in data-dir if empty.")

	// ── RTT tuning ────────────────────────────────────────────────────────────
	rttMs := flag.Uint64("rtt-ms", 200, "Estimated average RTT between nodes in milliseconds")

	flag.Parse()

	// ── Parse initial members ────────────────────────────────────────────────
	initialMembers := parseMembers(*members, *joinCluster)

	// ── Build NodeHostConfig ─────────────────────────────────────────────────
	nhCfg := config.NodeHostConfig{
		WALDir:         *dataDir,
		NodeHostDir:    *dataDir,
		RTTMillisecond: *rttMs,
		RaftAddress:    *raftAddr,
	}

	if *useGossip {
		validateGossipFlags(*gossipAddr, *gossipAdvert, *gossipSeeds)
		nhCfg.DefaultNodeRegistryEnabled = true
		nhCfg.NodeHostID = *nodeHostID // empty = auto-generate + persist
		nhCfg.Gossip = config.GossipConfig{
			BindAddress:      *gossipAddr,
			AdvertiseAddress: *gossipAdvert,
			Seed:             parseSeeds(*gossipSeeds),
		}
		log.Printf("Gossip mode enabled: bind=%s advertise=%s seeds=%s",
			*gossipAddr, *gossipAdvert, *gossipSeeds)
	}

	// ── Start NodeHost ───────────────────────────────────────────────────────
	nh, err := dragonboat.NewNodeHost(nhCfg)
	if err != nil {
		log.Fatalf("failed to create NodeHost: %v", err)
	}
	defer nh.Close()

	log.Printf("NodeHost ID: %s  RaftAddr: %s", nh.ID(), *raftAddr)

	// ── Start Raft shard ─────────────────────────────────────────────────────
	raftCfg := config.Config{
		ReplicaID:       *nodeID,
		ShardID:         *shardID,
		ElectionRTT:     10,
		HeartbeatRTT:    1,
		CheckQuorum:     true,
		PreVote:         true, // prevents spurious elections during transient partitions
		SnapshotEntries: 100,
		WaitReady:       true,
	}

	if err := nh.StartReplica(initialMembers, *joinCluster, statemachine.NewKVStateMachine, raftCfg); err != nil {
		log.Fatalf("failed to start replica: %v", err)
	}
	log.Printf("Raft replica started: shard=%d replica=%d", *shardID, *nodeID)

	// ── gRPC server ──────────────────────────────────────────────────────────
	grpcSrv := grpc.NewServer()
	dbpb.RegisterDragonboatServiceServer(grpcSrv, grpcserver.New(nh, *opTimeout))
	reflection.Register(grpcSrv) // enables grpcurl out of the box

	grpcLis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", *grpcAddr, err)
	}
	go func() {
		log.Printf("gRPC listening on %s", *grpcAddr)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			log.Printf("gRPC server stopped: %v", err)
		}
	}()

	// ── REST gateway ─────────────────────────────────────────────────────────
	ctx, cancelGateway := context.WithCancel(context.Background())
	defer cancelGateway()

	restHandler, err := gateway.NewHandler(ctx, *grpcAddr)
	if err != nil {
		log.Fatalf("failed to create REST gateway: %v", err)
	}
	restSrv := &http.Server{
		Addr:              *restAddr,
		Handler:           restHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("REST gateway listening on %s", *restAddr)
		if err := restSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("REST server stopped: %v", err)
		}
	}()

	// ── Banner ────────────────────────────────────────────────────────────────
	printBanner(nh.ID(), *grpcAddr, *restAddr, *useGossip, *gossipAdvert)

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("signal received, shutting down...")

	cancelGateway()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := restSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("REST shutdown error: %v", err)
	}
	grpcSrv.GracefulStop()
	log.Println("stopped cleanly")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// parseMembers converts the --initial-members string into the map required
// by NodeHost.StartReplica.  Returns an empty map (not nil) when s is blank,
// which tells Dragonboat to restart from persisted data.
func parseMembers(s string, join bool) map[uint64]string {
	m := map[uint64]string{}
	if join || s == "" {
		return m
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			log.Fatalf("invalid member spec %q — expected id=addr (or id=NodeHostID in gossip mode)", part)
		}
		id, err := strconv.ParseUint(kv[0], 10, 64)
		if err != nil {
			log.Fatalf("invalid member id %q: %v", kv[0], err)
		}
		m[id] = kv[1]
	}
	return m
}

// parseSeeds splits a comma-separated seed list, trimming whitespace.
func parseSeeds(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// validateGossipFlags aborts early if required gossip flags are missing.
func validateGossipFlags(bind, advertise, seeds string) {
	if bind == "" {
		log.Fatal("--gossip-addr is required when --gossip is set")
	}
	if advertise == "" {
		log.Fatal("--gossip-advertise is required when --gossip is set (must be a concrete IP:port reachable by other nodes)")
	}
	if seeds == "" {
		log.Fatal("--gossip-seeds is required when --gossip is set (comma-separated list of other nodes' gossip addresses)")
	}
}

// envOr returns the env var value or the fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBool returns true if the env var is set to a truthy value.
func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}

// printBanner prints a startup summary to stdout.
func printBanner(nhID, grpcAddr, restAddr string, gossip bool, gossipAdvertise string) {
	mode := "static"
	if gossip {
		mode = "gossip (dynamic)"
	}
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │            Dragonboat gRPC + REST Server                    │")
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	fmt.Printf("  │  NodeHost ID : %s\n", nhID)
	fmt.Printf("  │  Mode        : %s\n", mode)
	if gossip {
		fmt.Printf("  │  Gossip addr : %s\n", gossipAdvertise)
	}
	fmt.Printf("  │  gRPC        : %s\n", grpcAddr)
	fmt.Printf("  │  REST        : http://<host>%s/v1/...\n", restAddr)
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	fmt.Println("  │  Quick test:                                                 │")
	fmt.Printf("  │    curl http://localhost%s/healthz\n", restAddr)
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")
	fmt.Println()
}
