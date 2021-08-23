// Copyright 2020 MatrixOrigin.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"github.com/matrixorigin/matrixcube/pb/bhmetapb"
)

// doCreateDynamically When we call the prophet client to dynamically create a shard,
// the watcher will receive the creation command, and this callback will be triggered.
// Called in prophet event handle goroutine.
func (s *store) doDynamicallyCreate(shard bhmetapb.Shard) {
	if _, ok := s.replicas.Load(shard.ID); ok {
		return
	}

	pr, err := createPeerReplica(s, &shard)
	if err != nil {
		s.revokeWorker(pr)
		return
	}

	s.addPR(pr)
	for _, p := range shard.Peers {
		s.peers.Store(p.ID, p)
	}
	s.mustSaveShards(shard)
	s.updateShardKeyRange(shard)
}