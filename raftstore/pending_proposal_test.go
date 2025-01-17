// Copyright 2021 MatrixOrigin.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless assertd by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"testing"

	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/pb/errorpb"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/util/leaktest"
	"github.com/matrixorigin/matrixcube/util/uuid"
	"github.com/stretchr/testify/assert"
)

func TestPendingProposalsCanBeCreated(t *testing.T) {
	defer leaktest.AfterTest(t)()

	p := newPendingProposals()
	assert.Empty(t, p.cmds)
	assert.Equal(t, batch{}, p.confChangeCmd)
}

func TestPendingProposalAppend(t *testing.T) {
	defer leaktest.AfterTest(t)()

	p := newPendingProposals()
	p.append(batch{})
	p.append(batch{})
	assert.Equal(t, 2, len(p.cmds))
}

func TestPendingProposalPop(t *testing.T) {
	defer leaktest.AfterTest(t)()

	p := newPendingProposals()
	cmd1 := batch{byteSize: 100}
	cmd2 := batch{byteSize: 200}
	p.append(cmd1)
	p.append(cmd2)
	assert.Equal(t, 2, len(p.cmds))
	v1, ok := p.pop()
	assert.True(t, ok)
	assert.Equal(t, 1, len(p.cmds))
	assert.Equal(t, cmd1, v1)
	v2, ok := p.pop()
	assert.True(t, ok)
	assert.Equal(t, 0, len(p.cmds))
	assert.Equal(t, cmd2, v2)
	_, ok = p.pop()
	assert.False(t, ok)
}

func TestPendingConfigChangeProposalCanBeSetAndGet(t *testing.T) {
	defer leaktest.AfterTest(t)()

	p := newPendingProposals()
	cmd := newTestBatch("", "", uint64(rpcpb.AdminConfigChange), rpcpb.Admin, 0, nil)
	p.setConfigChange(cmd)
	v := p.getConfigChange()
	assert.Equal(t, cmd, v)
}

func TestPendingProposalWontAcceptRegularCmdAsConfigChanageCmd(t *testing.T) {
	defer leaktest.AfterTest(t)()

	cmd := newTestBatch("", "", uint64(rpcpb.AdminTransferLeader), rpcpb.Admin, 0, nil)
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("failed to trigger panic")
		}
	}()
	p := newPendingProposals()
	p.setConfigChange(cmd)
}

func testPendingProposalClear(t *testing.T,
	clear bool, cb func(resp rpcpb.ResponseBatch)) {
	cmd1 := batch{
		logger: log.Adjust(nil),
		requestBatch: rpcpb.RequestBatch{
			Requests: []rpcpb.Request{{}},
			Header: rpcpb.RequestBatchHeader{
				ID: uuid.NewV4().Bytes(),
			},
		},
		cb: cb,
	}
	cmd2 := batch{
		logger: log.Adjust(nil),
		requestBatch: rpcpb.RequestBatch{
			Requests: []rpcpb.Request{{}},
			Header: rpcpb.RequestBatchHeader{
				ID: uuid.NewV4().Bytes(),
			},
		},
		cb: cb,
	}

	ConfChangeCmd := batch{
		logger:       log.Adjust(nil),
		requestBatch: newTestAdminRequestBatch(string(uuid.NewV4().Bytes()), 0, rpcpb.AdminConfigChange, nil),
		cb:           cb,
	}
	p := newPendingProposals()
	p.append(cmd1)
	p.append(cmd2)
	p.setConfigChange(ConfChangeCmd)
	if clear {
		p.clear()
	} else {
		p.close()
	}
	assert.Empty(t, p.cmds)
	assert.Equal(t, emptyCMD, p.confChangeCmd)
}

func TestPendingProposalClear(t *testing.T) {
	defer leaktest.AfterTest(t)()

	check := func(resp rpcpb.ResponseBatch) {
		assert.Equal(t, 1, len(resp.Responses))
		assert.Equal(t, errStaleCMD.Error(), resp.Header.Error.Message)
	}
	testPendingProposalClear(t, true, check)
}

func TestPendingProposalDestroy(t *testing.T) {
	defer leaktest.AfterTest(t)()

	check := func(resp rpcpb.ResponseBatch) {
		assert.Equal(t, 1, len(resp.Responses))
		assert.Equal(t, errShardNotFound.Error(), resp.Responses[0].Error.Message)
	}
	testPendingProposalClear(t, false, check)
}

func TestPendingProposalCanNotifyConfigChangeCmd(t *testing.T) {
	defer leaktest.AfterTest(t)()

	called := false
	cb := func(resp rpcpb.ResponseBatch) {
		called = true
		assert.Equal(t, 1, len(resp.Responses))
		assert.Equal(t, errStaleCMD.Error(), resp.Header.Error.Message)
	}
	ConfChangeCmd := batch{
		logger:       log.Adjust(nil),
		requestBatch: newTestAdminRequestBatch(string(uuid.NewV4().Bytes()), 0, rpcpb.AdminConfigChange, nil),
		cb:           cb,
	}
	p := newPendingProposals()
	p.setConfigChange(ConfChangeCmd)
	resp := errorStaleCMDResp(ConfChangeCmd.getRequestID())
	p.notify(ConfChangeCmd.requestBatch.Header.ID, resp, true)
	assert.True(t, called)
	assert.Equal(t, emptyCMD, p.confChangeCmd)
}

func TestPendingProposalCanNotifyRegularCmd(t *testing.T) {
	defer leaktest.AfterTest(t)()

	called := false
	staleCalled := false
	staleCB := func(resp rpcpb.ResponseBatch) {
		staleCalled = true
		assert.Equal(t, 1, len(resp.Responses))
		assert.Equal(t, errStaleCMD.Error(), resp.Header.Error.Message)
	}
	cb := func(resp rpcpb.ResponseBatch) {
		called = true
		assert.Equal(t, 1, len(resp.Responses))
		assert.Equal(t, errShardNotFound.Error(), resp.Header.Error.Message)
	}
	cmd1 := batch{
		logger: log.Adjust(nil),
		requestBatch: rpcpb.RequestBatch{
			Requests: []rpcpb.Request{{}},
			Header: rpcpb.RequestBatchHeader{
				ID: uuid.NewV4().Bytes(),
			},
		},
		cb: staleCB,
	}
	cmd2 := batch{
		logger: log.Adjust(nil),
		requestBatch: rpcpb.RequestBatch{
			Requests: []rpcpb.Request{{}},
			Header: rpcpb.RequestBatchHeader{
				ID: uuid.NewV4().Bytes(),
			},
		},
		cb: cb,
	}
	cmd3 := batch{logger: log.Adjust(nil)}
	p := newPendingProposals()
	p.append(cmd1)
	p.append(cmd2)
	p.append(cmd3)

	err := new(errorpb.ShardNotFound)
	err.ShardID = 100
	resp := errorPbResp(cmd2.requestBatch.Header.ID, errorpb.Error{
		Message:       errShardNotFound.Error(),
		ShardNotFound: err,
	})
	p.notify(cmd2.requestBatch.Header.ID, resp, false)
	assert.True(t, called)
	assert.True(t, staleCalled)
	assert.Equal(t, 1, len(p.cmds))

	v, ok := p.pop()
	assert.True(t, ok)
	assert.Equal(t, cmd3, v)
}
