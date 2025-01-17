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
	"errors"
	"testing"

	"github.com/matrixorigin/matrixcube/pb/metapb"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/util/leaktest"
	"github.com/matrixorigin/matrixcube/util/uuid"
	"github.com/stretchr/testify/assert"
)

func TestEpochMatch(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		confVer1 uint64
		version1 uint64
		confVer2 uint64
		version2 uint64
		match    bool
	}{
		{1, 1, 1, 1, true},
		{1, 1, 1, 2, false},
		{1, 1, 2, 1, false},
		{1, 1, 2, 2, false},
	}

	for _, tt := range tests {
		e1 := metapb.ShardEpoch{
			ConfigVer:  tt.confVer1,
			Generation: tt.version1,
		}
		e2 := metapb.ShardEpoch{
			ConfigVer:  tt.confVer2,
			Generation: tt.version2,
		}
		assert.Equal(t, tt.match, epochMatch(e1, e2))
	}
}

func TestCanAppendCmd(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		ignored1  bool
		confVer1  uint64
		version1  uint64
		ignored2  bool
		confVer2  uint64
		version2  uint64
		canAppend bool
	}{
		{true, 1, 1, true, 1, 1, true},
		{true, 1, 1, true, 2, 2, true},
		{true, 1, 1, false, 1, 1, false},
		{true, 1, 1, false, 2, 2, false},
		{false, 1, 1, false, 1, 1, true},
		{false, 1, 1, false, 1, 2, false},
		{false, 1, 1, false, 2, 1, false},
		{false, 1, 1, false, 2, 2, false},
	}

	for _, tt := range tests {
		cmd := &batch{
			requestBatch: rpcpb.RequestBatch{
				Requests: []rpcpb.Request{
					{
						Epoch: metapb.ShardEpoch{
							ConfigVer:  tt.confVer1,
							Generation: tt.version1,
						},
						IgnoreEpochCheck: tt.ignored1,
					},
				},
			},
		}
		req := rpcpb.Request{
			Epoch: metapb.ShardEpoch{
				ConfigVer:  tt.confVer2,
				Generation: tt.version2,
			},
			IgnoreEpochCheck: tt.ignored2,
		}
		assert.Equal(t, tt.canAppend, cmd.canBatches(req))
	}
}

func TestBatchResp(t *testing.T) {
	defer leaktest.AfterTest(t)()

	b := newTestBatch("id", "key", 1, rpcpb.Read, 2, func(rb rpcpb.ResponseBatch) {
		assert.True(t, rb.Header.IsEmpty())
		assert.Equal(t, 1, len(rb.Responses))

		rsp := rb.Responses[0]
		assert.Equal(t, "id", string(rsp.ID))
		assert.Equal(t, int64(2), rsp.PID)
		assert.Equal(t, rpcpb.Read, rsp.Type)
		assert.Equal(t, "value", string(rsp.Value))
	})
	b.resp(rpcpb.ResponseBatch{Responses: []rpcpb.Response{{Value: []byte("value")}}})
}

func TestBatchRespWithError(t *testing.T) {
	defer leaktest.AfterTest(t)()

	b := newTestBatch("id", "key", 1, rpcpb.Read, 2, func(rb rpcpb.ResponseBatch) {
		assert.False(t, rb.Header.IsEmpty())
		assert.Equal(t, 1, len(rb.Responses))

		rsp := rb.Responses[0]
		assert.Equal(t, "id", string(rsp.ID))
		assert.Equal(t, int64(2), rsp.PID)
		assert.Equal(t, rpcpb.Read, rsp.Type)
		assert.Empty(t, rsp.Value)
		assert.Equal(t, errorOtherCMDResp(errors.New("error resp")).Header.Error, rsp.Error)
	})

	b.resp(errorOtherCMDResp(errors.New("error resp")))
}

func TestAdminResp(t *testing.T) {
	defer leaktest.AfterTest(t)()

	resp := rpcpb.BatchSplitResponse{Shards: []Shard{{ID: 1}}}
	b := newTestBatch("id", "", uint64(rpcpb.AdminBatchSplit), rpcpb.Admin, 1, func(rb rpcpb.ResponseBatch) {
		assert.True(t, rb.Header.IsEmpty())
		assert.Equal(t, 1, len(rb.Responses))
		assert.True(t, rb.IsAdmin())
		assert.Equal(t, rpcpb.AdminBatchSplit, rb.GetAdminCmdType())
		assert.Equal(t, resp, rb.GetBatchSplitResponse())
	})
	b.resp(newAdminResponseBatch(rpcpb.AdminBatchSplit, &resp))
}

func TestAdminRespWithError(t *testing.T) {
	defer leaktest.AfterTest(t)()

	b := newTestBatch("id", "", uint64(rpcpb.AdminBatchSplit), rpcpb.Admin, 1, func(rb rpcpb.ResponseBatch) {
		assert.False(t, rb.Header.IsEmpty())
		assert.Equal(t, 1, len(rb.Responses))
		assert.True(t, rb.IsAdmin())
		assert.Equal(t, rpcpb.AdminBatchSplit, rb.GetAdminCmdType())
		assert.Equal(t, errorOtherCMDResp(errors.New("error resp")).Header.Error, rb.Header.Error)
	})
	b.resp(errorOtherCMDResp(errors.New("error resp")))
}

func newTestBatch(id string, key string, customType uint64, cmdType rpcpb.CmdType, pid int64, cb func(rpcpb.ResponseBatch)) batch {
	return newBatch(nil,
		rpcpb.RequestBatch{
			Header: rpcpb.RequestBatchHeader{ID: uuid.NewV4().Bytes()},
			Requests: []rpcpb.Request{
				{
					ID:         []byte(id),
					PID:        pid,
					Key:        []byte(key),
					CustomType: customType,
					Type:       cmdType,
				},
			},
		},
		cb,
		0,
		0)
}

func newTestAdminRequestBatch(id string, pid int64, cmdType rpcpb.AdminCmdType, cmd []byte) rpcpb.RequestBatch {
	return rpcpb.RequestBatch{
		Header: rpcpb.RequestBatchHeader{ID: uuid.NewV4().Bytes()},
		Requests: []rpcpb.Request{
			{
				ID:         []byte(id),
				PID:        pid,
				CustomType: uint64(cmdType),
				Type:       rpcpb.Admin,
				Cmd:        cmd,
			},
		},
	}
}

// TODO: add more tests for cmd.go
