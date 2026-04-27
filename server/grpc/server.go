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

// Package grpcserver provides a gRPC server that exposes NodeHost operations
// to external clients. Every method is a thin, synchronous wrapper around the
// corresponding NodeHost API call.
package grpcserver

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dragonboat "github.com/symbioticllc/caduceus/v4"
	dbpb "github.com/symbioticllc/caduceus/v4/server/proto"
)

const defaultTimeout = 5 * time.Second

// Server implements dbpb.DragonboatServiceServer by delegating every call to
// an underlying *dragonboat.NodeHost instance.
type Server struct {
	dbpb.UnimplementedDragonboatServiceServer

	nh      *dragonboat.NodeHost
	timeout time.Duration
}

// New creates a Server wrapping the given NodeHost.
// timeout controls how long each operation will wait for Raft consensus;
// pass 0 to use the default (5 s).
func New(nh *dragonboat.NodeHost, timeout time.Duration) *Server {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Server{nh: nh, timeout: timeout}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// resolveTimeout returns the per-request timeout when set (> 0), otherwise
// falls back to the server default.
func (s *Server) resolveTimeout(ms uint64) time.Duration {
	if ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return s.timeout
}

// withTimeout attaches a deadline to ctx using the resolved timeout.
func (s *Server) withTimeout(parent context.Context, ms uint64) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.resolveTimeout(ms))
}

// mapError converts known Dragonboat errors to appropriate gRPC status codes.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch err {
	case dragonboat.ErrShardNotFound:
		return status.Errorf(codes.NotFound, "shard not found: %v", err)
	case dragonboat.ErrClosed:
		return status.Errorf(codes.Unavailable, "node host closed: %v", err)
	case dragonboat.ErrDeadlineNotSet, dragonboat.ErrInvalidDeadline:
		return status.Errorf(codes.InvalidArgument, "invalid deadline: %v", err)
	case dragonboat.ErrShardAlreadyExist:
		return status.Errorf(codes.AlreadyExists, "%v", err)
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

// ─── Data Operations ──────────────────────────────────────────────────────────

// Propose submits a command to the Raft shard and waits for it to be applied.
func (s *Server) Propose(ctx context.Context, req *dbpb.ProposeRequest) (*dbpb.ProposeResponse, error) {
	tctx, cancel := s.withTimeout(ctx, req.TimeoutMs)
	defer cancel()

	session := s.nh.GetNoOPSession(req.ShardId)
	result, err := s.nh.SyncPropose(tctx, session, req.Command)
	if err != nil {
		return nil, mapError(err)
	}
	return &dbpb.ProposeResponse{Value: result.Value, Data: result.Data}, nil
}

// Read performs a linearisable read against the state machine.
func (s *Server) Read(ctx context.Context, req *dbpb.ReadRequest) (*dbpb.ReadResponse, error) {
	tctx, cancel := s.withTimeout(ctx, req.TimeoutMs)
	defer cancel()

	result, err := s.nh.SyncRead(tctx, req.ShardId, req.Query)
	if err != nil {
		return nil, mapError(err)
	}
	data, err := toBytes(result)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "result encoding: %v", err)
	}
	return &dbpb.ReadResponse{Data: data}, nil
}

// StaleRead performs a non-linearisable local read (lower latency, may return
// slightly stale data).
func (s *Server) StaleRead(ctx context.Context, req *dbpb.ReadRequest) (*dbpb.ReadResponse, error) {
	result, err := s.nh.StaleRead(req.ShardId, req.Query)
	if err != nil {
		return nil, mapError(err)
	}
	data, err := toBytes(result)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "result encoding: %v", err)
	}
	return &dbpb.ReadResponse{Data: data}, nil
}

// ─── Membership ───────────────────────────────────────────────────────────────

// GetMembership returns the current Raft membership of the specified shard.
func (s *Server) GetMembership(ctx context.Context, req *dbpb.GetMembershipRequest) (*dbpb.GetMembershipResponse, error) {
	tctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	m, err := s.nh.SyncGetShardMembership(tctx, req.ShardId)
	if err != nil {
		return nil, mapError(err)
	}

	resp := &dbpb.GetMembershipResponse{
		ConfigChangeId: m.ConfigChangeID,
		Nodes:          m.Nodes,
		NonVotings:     m.NonVotings,
		Witnesses:      m.Witnesses,
	}
	for id := range m.Removed {
		resp.Removed = append(resp.Removed, id)
	}
	return resp, nil
}

// AddReplica adds a new voting replica to the specified shard.
func (s *Server) AddReplica(ctx context.Context, req *dbpb.AddReplicaRequest) (*dbpb.AddReplicaResponse, error) {
	tctx, cancel := s.withTimeout(ctx, req.TimeoutMs)
	defer cancel()

	err := s.nh.SyncRequestAddReplica(tctx, req.ShardId, req.ReplicaId, req.Target, req.ConfigChangeId)
	if err != nil {
		return nil, mapError(err)
	}
	return &dbpb.AddReplicaResponse{}, nil
}

// RemoveReplica removes a replica from the specified shard.
func (s *Server) RemoveReplica(ctx context.Context, req *dbpb.RemoveReplicaRequest) (*dbpb.RemoveReplicaResponse, error) {
	tctx, cancel := s.withTimeout(ctx, req.TimeoutMs)
	defer cancel()

	err := s.nh.SyncRequestDeleteReplica(tctx, req.ShardId, req.ReplicaId, req.ConfigChangeId)
	if err != nil {
		return nil, mapError(err)
	}
	return &dbpb.RemoveReplicaResponse{}, nil
}

// TransferLeader requests that leadership of the shard be moved to the
// specified target replica.
func (s *Server) TransferLeader(_ context.Context, req *dbpb.TransferLeaderRequest) (*dbpb.TransferLeaderResponse, error) {
	if err := s.nh.RequestLeaderTransfer(req.ShardId, req.TargetReplicaId); err != nil {
		return nil, mapError(err)
	}
	return &dbpb.TransferLeaderResponse{}, nil
}

// ─── Maintenance ─────────────────────────────────────────────────────────────

// RequestSnapshot triggers an immediate snapshot of the specified shard.
func (s *Server) RequestSnapshot(ctx context.Context, req *dbpb.SnapshotRequest) (*dbpb.SnapshotResponse, error) {
	tctx, cancel := s.withTimeout(ctx, req.TimeoutMs)
	defer cancel()

	idx, err := s.nh.SyncRequestSnapshot(tctx, req.ShardId, dragonboat.DefaultSnapshotOption)
	if err != nil {
		return nil, mapError(err)
	}
	return &dbpb.SnapshotResponse{SnapshotIndex: idx}, nil
}

// ─── Observability ───────────────────────────────────────────────────────────

// GetNodeHostInfo returns status information for the local NodeHost instance.
func (s *Server) GetNodeHostInfo(_ context.Context, req *dbpb.NodeHostInfoRequest) (*dbpb.NodeHostInfoResponse, error) {
	opt := dragonboat.DefaultNodeHostInfoOption
	opt.SkipLogInfo = req.SkipLogInfo
	info := s.nh.GetNodeHostInfo(opt)

	resp := &dbpb.NodeHostInfoResponse{
		NodeHostId:  info.NodeHostID,
		RaftAddress: info.RaftAddress,
	}
	for _, si := range info.ShardInfoList {
		nodes := make(map[uint64]string, len(si.Replicas))
		for id, addr := range si.Replicas {
			nodes[id] = addr
		}
		resp.Shards = append(resp.Shards, &dbpb.ShardInfo{
			ShardId:   si.ShardID,
			ReplicaId: si.ReplicaID,
			IsLeader:  si.IsLeader,
			LeaderId:  si.LeaderID,
			Nodes:     nodes,
		})
	}
	return resp, nil
}

// GetLeader returns the current leader of the specified shard.
func (s *Server) GetLeader(_ context.Context, req *dbpb.GetLeaderRequest) (*dbpb.GetLeaderResponse, error) {
	leaderID, term, valid, err := s.nh.GetLeaderID(req.ShardId)
	if err != nil {
		return nil, mapError(err)
	}
	return &dbpb.GetLeaderResponse{
		LeaderId:  leaderID,
		Term:      term,
		Available: valid,
	}, nil
}

// HealthCheck returns OK when the NodeHost is alive.
func (s *Server) HealthCheck(_ context.Context, _ *dbpb.HealthCheckRequest) (*dbpb.HealthCheckResponse, error) {
	return &dbpb.HealthCheckResponse{
		Status:      "OK",
		NodeHostId:  s.nh.ID(),
	}, nil
}

// ─── util ─────────────────────────────────────────────────────────────────────

// toBytes converts a state machine Lookup result to a []byte.
// If result is already []byte, it is returned as-is.
func toBytes(v interface{}) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case []byte:
		return t, nil
	case string:
		return []byte(t), nil
	default:
		return nil, fmt.Errorf("cannot convert %T to []byte", v)
	}
}
