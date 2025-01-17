// Copyright 2021 MatrixOrigin.
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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"

	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/config"
	"github.com/matrixorigin/matrixcube/logdb"
	"github.com/matrixorigin/matrixcube/pb/metapb"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/util/leaktest"
)

func TestGetConfigChangeKind(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		changeNum int
		kind      confChangeKind
	}{
		{0, leaveJointKind},
		{1, simpleKind},
		{2, enterJointKind},
		{3, enterJointKind},
		{4, enterJointKind},
		{100, enterJointKind},
		{math.MaxInt, enterJointKind},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.kind, getConfigChangeKind(tt.changeNum))
	}
}

func TestIsValidConfigChangeRequest(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		ct      metapb.ConfigChangeType
		replica metapb.Replica
		valid   bool
	}{
		{
			metapb.ConfigChangeType_RemoveNode,
			metapb.Replica{},
			true,
		},
		{
			metapb.ConfigChangeType_AddNode,
			metapb.Replica{Role: metapb.ReplicaRole_Voter},
			true,
		},
		{
			metapb.ConfigChangeType_AddNode,
			metapb.Replica{Role: metapb.ReplicaRole_Learner},
			false,
		},
		{
			metapb.ConfigChangeType_AddLearnerNode,
			metapb.Replica{Role: metapb.ReplicaRole_Learner},
			true,
		},
		{
			metapb.ConfigChangeType_AddLearnerNode,
			metapb.Replica{Role: metapb.ReplicaRole_Voter},
			false,
		},
	}

	for _, tt := range tests {
		ccr := rpcpb.ConfigChangeRequest{
			ChangeType: tt.ct,
			Replica:    tt.replica,
		}
		assert.Equal(t, tt.valid, isValidConfigChangeRequest(ccr))
	}
}

func TestIsRemovingOrDemotingLeader(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		kind            confChangeKind
		leaderReplicaID uint64
		replicaID       uint64
		ct              metapb.ConfigChangeType
		result          bool
	}{
		{simpleKind, 1, 2, metapb.ConfigChangeType_RemoveNode, false},
		{simpleKind, 2, 2, metapb.ConfigChangeType_RemoveNode, true},
		{simpleKind, 2, 2, metapb.ConfigChangeType_AddLearnerNode, true},
		{leaveJointKind, 2, 2, metapb.ConfigChangeType_AddLearnerNode, false},
		{enterJointKind, 2, 2, metapb.ConfigChangeType_AddLearnerNode, false},
	}

	for _, tt := range tests {
		ccr := rpcpb.ConfigChangeRequest{
			ChangeType: tt.ct,
			Replica: metapb.Replica{
				ID: tt.replicaID,
			},
		}
		assert.Equal(t, tt.result,
			isRemovingOrDemotingLeader(tt.kind, ccr, tt.leaderReplicaID))
	}
}

func TestRemovingVoterDirectlyInJointConsensusCC(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		kind   confChangeKind
		ct     metapb.ConfigChangeType
		role   metapb.ReplicaRole
		result bool
	}{
		{
			enterJointKind,
			metapb.ConfigChangeType_RemoveNode,
			metapb.ReplicaRole_Voter,
			true,
		},
		{
			enterJointKind,
			metapb.ConfigChangeType_RemoveNode,
			metapb.ReplicaRole_Learner,
			false,
		},
		{
			enterJointKind,
			metapb.ConfigChangeType_AddLearnerNode,
			metapb.ReplicaRole_Learner,
			false,
		},
		{
			simpleKind,
			metapb.ConfigChangeType_RemoveNode,
			metapb.ReplicaRole_Voter,
			false,
		},
	}

	for _, tt := range tests {
		ccr := rpcpb.ConfigChangeRequest{
			ChangeType: tt.ct,
			Replica: metapb.Replica{
				Role: tt.role,
			},
		}

		assert.Equal(t, tt.result,
			removingVoterDirectlyInJointConsensusCC(tt.kind, ccr))
	}
}

func TestGetRequestTypeWillPanicWhenBatchHasBothReadWrite(t *testing.T) {
	defer leaktest.AfterTest(t)()

	batch := rpcpb.RequestBatch{
		Requests: []rpcpb.Request{
			{
				Type: rpcpb.Write,
			},
			{
				Type: rpcpb.Read,
			},
		},
	}
	require.False(t, batch.IsAdmin())
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("failed to trigger panic")
		}
	}()
	r := replica{}
	r.getRequestType(batch)
}

func TestGetRequestType(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		req rpcpb.RequestBatch
		rt  requestType
	}{
		{
			rpcpb.RequestBatch{
				Requests: []rpcpb.Request{
					{
						Type: rpcpb.Write,
					},
				},
			},
			proposalNormal,
		},
		{
			rpcpb.RequestBatch{
				Requests: []rpcpb.Request{
					{
						Type: rpcpb.Read,
					},
				},
			},
			readIndex,
		},
		{
			newTestAdminRequestBatch("", 0, rpcpb.AdminConfigChange, nil),
			proposalConfigChange,
		},
		{
			newTestAdminRequestBatch("", 0, rpcpb.AdminTransferLeader, nil),
			requestTransferLeader,
		},
		{
			newTestAdminRequestBatch("", 0, rpcpb.AdminBatchSplit, nil),
			proposalNormal,
		},
	}

	for _, tt := range tests {
		r := replica{}
		assert.Equal(t, tt.rt, r.getRequestType(tt.req))
	}
}

func TestToConfigChangeIV1(t *testing.T) {
	defer leaktest.AfterTest(t)()

	req := rpcpb.ConfigChangeRequest{
		ChangeType: metapb.ConfigChangeType_RemoveNode,
		Replica: metapb.Replica{
			ID: 123,
		},
	}
	p := replica{}
	data := make([]byte, 8)
	data[0] = 0x23
	data[7] = 0xbf
	cci := p.toConfChangeI(req, data)
	cc, ok := cci.(*raftpb.ConfChange)
	require.True(t, ok)
	assert.Equal(t, raftpb.ConfChangeType(req.ChangeType), cc.Type)
	assert.Equal(t, req.Replica.ID, cc.NodeID)
	assert.Equal(t, data, cc.Context)
}

func TestInvalidConfigChangeRequestIsRejected(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		req rpcpb.ConfigChangeRequest
		err error
	}{
		{
			rpcpb.ConfigChangeRequest{
				ChangeType: metapb.ConfigChangeType_RemoveNode,
				Replica: metapb.Replica{
					ID: 100,
				},
			},
			nil,
		},
		{
			rpcpb.ConfigChangeRequest{
				ChangeType: metapb.ConfigChangeType_AddNode,
				Replica: metapb.Replica{
					Role: metapb.ReplicaRole_Voter,
					ID:   100,
				},
			},
			nil,
		},
		{
			rpcpb.ConfigChangeRequest{
				ChangeType: metapb.ConfigChangeType_AddLearnerNode,
				Replica: metapb.Replica{
					Role: metapb.ReplicaRole_Learner,
					ID:   100,
				},
			},
			nil,
		},
		{
			rpcpb.ConfigChangeRequest{
				ChangeType: metapb.ConfigChangeType_AddNode,
				Replica: metapb.Replica{
					Role: metapb.ReplicaRole_Learner,
					ID:   100,
				},
			},
			ErrInvalidConfigChangeRequest,
		},
		{
			rpcpb.ConfigChangeRequest{
				ChangeType: metapb.ConfigChangeType_AddLearnerNode,
				Replica: metapb.Replica{
					Role: metapb.ReplicaRole_Voter,
					ID:   100,
				},
			},
			ErrInvalidConfigChangeRequest,
		},
		{
			rpcpb.ConfigChangeRequest{
				ChangeType: metapb.ConfigChangeType_RemoveNode,
				Replica: metapb.Replica{
					ID: 1,
				},
			},
			ErrRemoveLeader,
		},
		{
			rpcpb.ConfigChangeRequest{
				ChangeType: metapb.ConfigChangeType_AddLearnerNode,
				Replica: metapb.Replica{
					Role: metapb.ReplicaRole_Learner,
					ID:   1,
				},
			},
			ErrRemoveLeader,
		},
	}

	for idx, tt := range tests {
		data := make([]byte, 8)
		data[0] = 0x23
		data[7] = 0xbf
		l := log.GetDefaultZapLogger()
		r := replica{
			store:     &store{cfg: &config.Config{}},
			replicaID: 1,
			replica: metapb.Replica{
				ID: 1,
			},
		}
		kv := getTestStorage()
		defer kv.Close()

		ldb := logdb.NewKVLogDB(kv, log.GetDefaultZapLogger())
		defer ldb.Close()

		c := &raft.Config{
			ID:              1,
			ElectionTick:    10,
			HeartbeatTick:   1,
			Storage:         NewLogReader(l, 1, 1, ldb),
			MaxInflightMsgs: 100,
			CheckQuorum:     true,
			PreVote:         true,
		}
		rn, err := raft.NewRawNode(c)
		require.NoError(t, err)
		r.rn = rn

		r.rn.ApplyConfChange(raftpb.ConfChange{
			Type:   raftpb.ConfChangeType(metapb.ConfigChangeType_AddNode),
			NodeID: 200,
		})

		cci := r.toConfChangeI(tt.req, data)
		result := r.checkConfChange([]rpcpb.ConfigChangeRequest{tt.req}, cci)
		assert.Equal(t, tt.err, result, "idx: %d", idx)
	}
}
