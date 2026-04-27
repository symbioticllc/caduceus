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

// Package statemachine provides a simple in-memory key-value IStateMachine
// implementation for use with the dragonboat gRPC/REST server layer.
// Commands are JSON-encoded KVCommand structs.
package statemachine

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	sm "github.com/symbioticllc/caduceus/v4/statemachine"
)

// Op identifies the operation type in a KVCommand.
type Op string

const (
	// OpSet sets key to value.
	OpSet Op = "set"
	// OpDelete removes key.
	OpDelete Op = "delete"
	// OpGet retrieves value for key (only valid in Lookup, not Update).
	OpGet Op = "get"
)

// KVCommand is the JSON wire format for commands passed through Raft.
type KVCommand struct {
	Op    Op     `json:"op"`
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// KVResult is the JSON wire format returned by Lookup.
type KVResult struct {
	Found bool   `json:"found"`
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// KVStateMachine is a simple in-memory key-value store backed by a map.
// It implements dragonboat's IStateMachine interface and is safe to use
// as a reference / demo state machine with the gRPC server layer.
type KVStateMachine struct {
	mu    sync.RWMutex
	store map[string]string
}

// NewKVStateMachine is the factory function compatible with
// sm.CreateStateMachineFunc.
func NewKVStateMachine(_ uint64, _ uint64) sm.IStateMachine {
	return &KVStateMachine{
		store: make(map[string]string),
	}
}

// Update applies a KVCommand to the state machine. It is called by Dragonboat
// after a proposal has been committed and must be deterministic.
func (s *KVStateMachine) Update(entry sm.Entry) (sm.Result, error) {
	var cmd KVCommand
	if err := json.Unmarshal(entry.Cmd, &cmd); err != nil {
		// Return an error result without crashing the state machine.
		return sm.Result{Value: 0, Data: []byte(err.Error())}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch cmd.Op {
	case OpSet:
		s.store[cmd.Key] = cmd.Value
		return sm.Result{Value: 1}, nil
	case OpDelete:
		_, existed := s.store[cmd.Key]
		delete(s.store, cmd.Key)
		if existed {
			return sm.Result{Value: 1}, nil
		}
		return sm.Result{Value: 0}, nil
	default:
		return sm.Result{Value: 0, Data: []byte(fmt.Sprintf("unknown op: %s", cmd.Op))}, nil
	}
}

// Lookup handles a read query. query must be a *KVCommand with Op == OpGet.
func (s *KVStateMachine) Lookup(query interface{}) (interface{}, error) {
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

	s.mu.RLock()
	val, ok := s.store[cmd.Key]
	s.mu.RUnlock()

	return json.Marshal(KVResult{Found: ok, Value: val})
}

// SaveSnapshot serialises the current key-value map into the writer.
func (s *KVStateMachine) SaveSnapshot(w io.Writer, _ sm.ISnapshotFileCollection, _ <-chan struct{}) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.NewEncoder(w).Encode(s.store)
}

// RecoverFromSnapshot restores state from a previously saved snapshot.
func (s *KVStateMachine) RecoverFromSnapshot(r io.Reader, _ []sm.SnapshotFile, _ <-chan struct{}) error {
	var store map[string]string
	if err := json.NewDecoder(r).Decode(&store); err != nil {
		return err
	}
	s.mu.Lock()
	s.store = store
	s.mu.Unlock()
	return nil
}

// Close releases all resources held by the KVStateMachine.
func (s *KVStateMachine) Close() error { return nil }
