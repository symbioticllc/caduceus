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

package statemachine

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/redis/go-redis/v9"

	sm "github.com/symbioticllc/caduceus/v4/statemachine"
)

// RedisConfig holds the connection settings for the Redis backend.
type RedisConfig struct {
	// Addr is the Redis server address (host:port). Default: localhost:6379.
	Addr string
	// Password is the Redis AUTH password. Leave empty for no auth.
	Password string
	// DB is the Redis logical database index. Default: 0.
	DB int
	// KeyPrefix namespaces all keys written by this state machine.
	// Default: "db:{shardID}:" — allows multiple shards to share one Redis.
	KeyPrefix string
}

// RedisStateMachine is a Dragonboat IStateMachine that persists its state in
// Redis. It accepts the same KVCommand JSON wire format as KVStateMachine so
// the gRPC/REST API layer requires no changes.
//
// Architecture note: each Dragonboat node uses its OWN Redis instance.
// Replication is performed by Raft, not by Redis. Sharing a Redis between
// nodes would bypass consensus entirely.
type RedisStateMachine struct {
	client    *redis.Client
	prefix    string
	clusterID uint64
	nodeID    uint64
}

// NewRedisStateMachineFactory returns a sm.CreateStateMachineFunc that connects
// to Redis using the supplied config. Call this once at startup and pass the
// result to NodeHost.StartReplica.
//
//	factory := statemachine.NewRedisStateMachineFactory(statemachine.RedisConfig{
//	    Addr: "localhost:6379",
//	})
//	nh.StartReplica(members, join, factory, raftCfg)
func NewRedisStateMachineFactory(cfg RedisConfig) sm.CreateStateMachineFunc {
	return func(clusterID, nodeID uint64) sm.IStateMachine {
		prefix := cfg.KeyPrefix
		if prefix == "" {
			prefix = fmt.Sprintf("db:%d:", clusterID)
		}
		addr := cfg.Addr
		if addr == "" {
			addr = "localhost:6379"
		}
		client := redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: cfg.Password,
			DB:       cfg.DB,
		})
		return &RedisStateMachine{
			client:    client,
			prefix:    prefix,
			clusterID: clusterID,
			nodeID:    nodeID,
		}
	}
}

// key returns the namespaced Redis key for the given logical key.
func (s *RedisStateMachine) key(k string) string {
	return s.prefix + k
}

// ctx returns a context with a generous deadline for Redis calls. Redis
// operations are expected to be sub-millisecond on localhost; 5 s is a
// conservative upper bound that won't interfere with Raft's own timeouts.
func (s *RedisStateMachine) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// ─── IStateMachine ────────────────────────────────────────────────────────────

// Update applies a KVCommand to Redis. It is called by Dragonboat after a
// proposal has been committed and MUST be deterministic.
func (s *RedisStateMachine) Update(entry sm.Entry) (sm.Result, error) {
	var cmd KVCommand
	if err := json.Unmarshal(entry.Cmd, &cmd); err != nil {
		// Return gracefully — a bad command must not crash the state machine.
		return sm.Result{Value: 0, Data: []byte(err.Error())}, nil
	}

	ctx, cancel := s.ctx()
	defer cancel()

	switch cmd.Op {
	case OpSet:
		if err := s.client.Set(ctx, s.key(cmd.Key), cmd.Value, 0).Err(); err != nil {
			return sm.Result{}, fmt.Errorf("redis SET: %w", err)
		}
		return sm.Result{Value: 1}, nil

	case OpDelete:
		n, err := s.client.Del(ctx, s.key(cmd.Key)).Result()
		if err != nil {
			return sm.Result{}, fmt.Errorf("redis DEL: %w", err)
		}
		return sm.Result{Value: uint64(n)}, nil

	default:
		return sm.Result{Value: 0, Data: []byte(fmt.Sprintf("unknown op: %s", cmd.Op))}, nil
	}
}

// Lookup performs a read against Redis. query must be a []byte containing a
// JSON-encoded KVCommand with Op == OpGet.
func (s *RedisStateMachine) Lookup(query interface{}) (interface{}, error) {
	var cmd KVCommand
	switch v := query.(type) {
	case []byte:
		if err := json.Unmarshal(v, &cmd); err != nil {
			return json.Marshal(KVResult{Error: err.Error()})
		}
	case *KVCommand:
		cmd = *v
	default:
		return json.Marshal(KVResult{Error: fmt.Sprintf("unexpected query type %T", query)})
	}

	ctx, cancel := s.ctx()
	defer cancel()

	val, err := s.client.Get(ctx, s.key(cmd.Key)).Result()
	if err == redis.Nil {
		return json.Marshal(KVResult{Found: false})
	}
	if err != nil {
		return json.Marshal(KVResult{Error: err.Error()})
	}
	return json.Marshal(KVResult{Found: true, Value: val})
}

// SaveSnapshot serialises the entire keyspace (all keys under this node's
// prefix) into the writer. Format: repeated frames of:
//
//	[4-byte big-endian key length][key bytes][8-byte big-endian value length][value bytes]
//
// We use Redis DUMP format for values so that TTLs and type info are preserved.
func (s *RedisStateMachine) SaveSnapshot(w io.Writer, _ sm.ISnapshotFileCollection, stop <-chan struct{}) error {
	ctx, cancel := s.ctx()
	defer cancel()

	bw := bufio.NewWriterSize(w, 1<<20) // 1 MiB write buffer

	var cursor uint64
	pattern := s.prefix + "*"
	for {
		select {
		case <-stop:
			return sm.ErrSnapshotStopped
		default:
		}

		keys, next, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return fmt.Errorf("redis SCAN: %w", err)
		}
		for _, fullKey := range keys {
			// Dump the value using Redis DUMP (binary serialisation, includes type+TTL).
			dump, err := s.client.Dump(ctx, fullKey).Result()
			if err != nil {
				return fmt.Errorf("redis DUMP %s: %w", fullKey, err)
			}
			// Strip our prefix so the key is portable across different prefix configs.
			logicalKey := fullKey[len(s.prefix):]
			if err := writeFrame(bw, []byte(logicalKey), []byte(dump)); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return bw.Flush()
}

// RecoverFromSnapshot restores state from a snapshot produced by SaveSnapshot.
// It first deletes all existing keys under the prefix, then RESTOREs each key.
func (s *RedisStateMachine) RecoverFromSnapshot(r io.Reader, _ []sm.SnapshotFile, stop <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Delete all existing keys in this node's keyspace.
	if err := s.flushPrefix(ctx); err != nil {
		return err
	}

	br := bufio.NewReaderSize(r, 1<<20)
	for {
		select {
		case <-stop:
			return sm.ErrSnapshotStopped
		default:
		}

		logicalKey, dump, err := readFrame(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		fullKey := s.key(string(logicalKey))
		// RESTORE key with no TTL (0 = keep original embedded TTL from DUMP).
		if err := s.client.RestoreReplace(ctx, fullKey, 0, string(dump)).Err(); err != nil {
			return fmt.Errorf("redis RESTORE %s: %w", fullKey, err)
		}
	}
	return nil
}

// Close closes the Redis client connection.
func (s *RedisStateMachine) Close() error {
	return s.client.Close()
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// flushPrefix deletes all keys that start with s.prefix using SCAN + DEL.
func (s *RedisStateMachine) flushPrefix(ctx context.Context) error {
	var cursor uint64
	pattern := s.prefix + "*"
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return fmt.Errorf("redis SCAN (flush): %w", err)
		}
		if len(keys) > 0 {
			if err := s.client.Del(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("redis DEL (flush): %w", err)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

// writeFrame writes one key-value pair in the snapshot wire format.
func writeFrame(w io.Writer, key, value []byte) error {
	klen := make([]byte, 4)
	binary.BigEndian.PutUint32(klen, uint32(len(key)))
	vlen := make([]byte, 8)
	binary.BigEndian.PutUint64(vlen, uint64(len(value)))

	for _, b := range [][]byte{klen, key, vlen, value} {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads one key-value pair from the snapshot wire format.
func readFrame(r io.Reader) (key, value []byte, err error) {
	var klenBuf [4]byte
	if _, err = io.ReadFull(r, klenBuf[:]); err != nil {
		return
	}
	klen := binary.BigEndian.Uint32(klenBuf[:])
	key = make([]byte, klen)
	if _, err = io.ReadFull(r, key); err != nil {
		return
	}

	var vlenBuf [8]byte
	if _, err = io.ReadFull(r, vlenBuf[:]); err != nil {
		return
	}
	vlen := binary.BigEndian.Uint64(vlenBuf[:])
	value = make([]byte, vlen)
	_, err = io.ReadFull(r, value)
	return
}
