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

package statemachine_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/symbioticllc/caduceus/v4/server/statemachine"
	sm "github.com/symbioticllc/caduceus/v4/statemachine"
)

// newTestRedis spins up an in-process Redis and returns a RedisStateMachine
// connected to it plus a cleanup function.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, statemachine.RedisConfig) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	cfg := statemachine.RedisConfig{Addr: mr.Addr()}
	return mr, cfg
}

func mustEntry(t *testing.T, op statemachine.Op, key, value string) sm.Entry {
	t.Helper()
	cmd := statemachine.KVCommand{Op: op, Key: key, Value: value}
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return sm.Entry{Cmd: b}
}

func mustLookup(t *testing.T, machine sm.IStateMachine, key string) statemachine.KVResult {
	t.Helper()
	q, _ := json.Marshal(statemachine.KVCommand{Op: statemachine.OpGet, Key: key})
	raw, err := machine.Lookup(q)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	var result statemachine.KVResult
	if err := json.Unmarshal(raw.([]byte), &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return result
}

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestRedis_SetAndGet(t *testing.T) {
	_, cfg := newTestRedis(t)
	factory := statemachine.NewRedisStateMachineFactory(cfg)
	m := factory(1, 1)
	defer m.Close()

	res, err := m.Update(mustEntry(t, statemachine.OpSet, "hello", "world"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Value != 1 {
		t.Errorf("set result.Value = %d, want 1", res.Value)
	}

	got := mustLookup(t, m, "hello")
	if !got.Found || got.Value != "world" {
		t.Errorf("lookup: found=%v value=%q, want found=true value=world", got.Found, got.Value)
	}
}

func TestRedis_Delete(t *testing.T) {
	_, cfg := newTestRedis(t)
	factory := statemachine.NewRedisStateMachineFactory(cfg)
	m := factory(1, 1)
	defer m.Close()

	m.Update(mustEntry(t, statemachine.OpSet, "gone", "soon"))

	res, err := m.Update(mustEntry(t, statemachine.OpDelete, "gone", ""))
	if err != nil {
		t.Fatalf("Update delete: %v", err)
	}
	if res.Value != 1 {
		t.Errorf("delete result.Value = %d, want 1", res.Value)
	}

	got := mustLookup(t, m, "gone")
	if got.Found {
		t.Errorf("key 'gone' still exists after delete")
	}
}

func TestRedis_DeleteMissing(t *testing.T) {
	_, cfg := newTestRedis(t)
	factory := statemachine.NewRedisStateMachineFactory(cfg)
	m := factory(1, 1)
	defer m.Close()

	res, err := m.Update(mustEntry(t, statemachine.OpDelete, "nonexistent", ""))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("delete missing result.Value = %d, want 0", res.Value)
	}
}

func TestRedis_LookupMissing(t *testing.T) {
	_, cfg := newTestRedis(t)
	factory := statemachine.NewRedisStateMachineFactory(cfg)
	m := factory(1, 1)
	defer m.Close()

	got := mustLookup(t, m, "nope")
	if got.Found {
		t.Errorf("expected not found for missing key")
	}
}

func TestRedis_KeyPrefix_Isolation(t *testing.T) {
	// Two state machines on the same Redis with different shard IDs must not
	// see each other's keys.
	_, cfg := newTestRedis(t)
	factory := statemachine.NewRedisStateMachineFactory(cfg)

	shard1 := factory(1, 1)
	shard2 := factory(2, 1)
	defer shard1.Close()
	defer shard2.Close()

	shard1.Update(mustEntry(t, statemachine.OpSet, "key", "from-shard-1"))
	shard2.Update(mustEntry(t, statemachine.OpSet, "key", "from-shard-2"))

	got1 := mustLookup(t, shard1, "key")
	got2 := mustLookup(t, shard2, "key")

	if got1.Value != "from-shard-1" {
		t.Errorf("shard1 value = %q, want from-shard-1", got1.Value)
	}
	if got2.Value != "from-shard-2" {
		t.Errorf("shard2 value = %q, want from-shard-2", got2.Value)
	}
}

func TestRedis_SnapshotRoundtrip(t *testing.T) {
	mr, cfg := newTestRedis(t)
	factory := statemachine.NewRedisStateMachineFactory(cfg)

	src := factory(1, 1)
	defer src.Close()

	// Write some state.
	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}} {
		if _, err := src.Update(mustEntry(t, statemachine.OpSet, kv[0], kv[1])); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	// Save snapshot.
	var buf bytes.Buffer
	stop := make(chan struct{})
	if err := src.SaveSnapshot(&buf, nil, stop); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// Restore into a fresh state machine on the same mini-redis.
	// Use a different shard ID so we don't collide.
	dst := factory(99, 1)
	defer dst.Close()

	// Manually patch the prefix for the restore target to match src so that
	// the snapshot (which strips the prefix) restores correctly.
	// We do this by creating a factory with an explicit KeyPrefix.
	cfg2 := cfg
	cfg2.KeyPrefix = "db:1:" // same prefix as src (shardID=1)
	factory2 := statemachine.NewRedisStateMachineFactory(cfg2)
	dst2 := factory2(0, 1) // clusterID=0 is ignored because prefix is explicit
	defer dst2.Close()

	// Flush mini-redis first so we start clean.
	mr.FlushAll()

	if err := dst2.RecoverFromSnapshot(&buf, nil, stop); err != nil {
		t.Fatalf("RecoverFromSnapshot: %v", err)
	}

	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}} {
		got := mustLookup(t, dst2, kv[0])
		if !got.Found || got.Value != kv[1] {
			t.Errorf("after restore: key=%q found=%v value=%q, want found=true value=%s",
				kv[0], got.Found, got.Value, kv[1])
		}
	}
}

func TestRedis_InvalidCommand(t *testing.T) {
	_, cfg := newTestRedis(t)
	factory := statemachine.NewRedisStateMachineFactory(cfg)
	m := factory(1, 1)
	defer m.Close()

	// Garbage bytes — should return a Result with error data, not panic.
	res, err := m.Update(sm.Entry{Cmd: []byte("not json")})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("invalid command result.Value = %d, want 0", res.Value)
	}
	if len(res.Data) == 0 {
		t.Errorf("expected error message in Data")
	}
}
