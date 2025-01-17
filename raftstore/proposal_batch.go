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
	"fmt"

	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/metric"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/util/buf"
	"github.com/matrixorigin/matrixcube/util/uuid"
	"go.uber.org/zap"
)

// TODO: request type should has its own type
const (
	read = iota
	write
	admin
)

var (
	emptyCMD = batch{}
	// testMaxProposalRequestCount just for test, how many requests can be aggregated in a batch, 0 is disabled
	testMaxProposalRequestCount = 0
)

type reqCtx struct {
	reqType int
	req     rpcpb.Request
	cb      func(rpcpb.ResponseBatch)
}

func newReqCtx(req rpcpb.Request, cb func(rpcpb.ResponseBatch)) reqCtx {
	ctx := reqCtx{req: req, cb: cb}
	switch req.Type {
	case rpcpb.Read:
		ctx.reqType = read
	case rpcpb.Write:
		ctx.reqType = write
	case rpcpb.Admin:
		ctx.reqType = admin
	default:
		panic(fmt.Sprintf("request context type %s not support", ctx.req.Type.String()))
	}
	return ctx
}

type proposalBatch struct {
	logger  *zap.Logger
	maxSize uint64
	shardID uint64
	replica Replica
	buf     *buf.ByteBuf
	batches []batch
}

func newProposalBatch(logger *zap.Logger, maxSize uint64, shardID uint64, replica Replica) *proposalBatch {
	return &proposalBatch{
		logger:  log.Adjust(logger),
		maxSize: maxSize,
		shardID: shardID,
		replica: replica,
		buf:     buf.NewByteBuf(512),
	}
}

func (b *proposalBatch) size() int {
	return len(b.batches)
}

func (b *proposalBatch) isEmpty() bool {
	return b.size() == 0
}

func (b *proposalBatch) pop() (batch, bool) {
	if b.isEmpty() {
		return emptyCMD, false
	}

	value := b.batches[0]
	b.batches[0] = emptyCMD
	b.batches = b.batches[1:]

	metric.SetRaftProposalBatchMetric(int64(len(value.requestBatch.Requests)))
	return value, true
}

func (b *proposalBatch) close() {
	for {
		if b.isEmpty() {
			break
		}
		if c, ok := b.pop(); ok {
			for _, req := range c.requestBatch.Requests {
				respStoreNotMatch(errStoreNotMatch, req, c.cb)
			}
		}
	}
}

// TODO: might make sense to move the epoch value into c.req

// push adds the specified req to a proposalBatch. The epoch value should
// reflect client's view of the shard when the request is made.
func (b *proposalBatch) push(group uint64, c reqCtx) {
	req := c.req
	cb := c.cb
	tp := c.reqType
	isAdmin := tp == admin

	// use data key to store
	if !isAdmin {
		b.buf.Clear()
	}

	n := req.Size()
	added := false
	if !isAdmin {
		for idx := range b.batches {
			if b.batches[idx].tp == tp && // only batches same type requests
				!b.batches[idx].isFull(n, int(b.maxSize)) && // check max batches size
				b.batches[idx].canBatches(req) { // check epoch field
				b.batches[idx].requestBatch.Requests = append(b.batches[idx].requestBatch.Requests, req)
				b.batches[idx].byteSize += n
				added = true
				break
			}
		}
	}

	if !added {
		rb := rpcpb.RequestBatch{}
		rb.Header.ShardID = b.shardID
		rb.Header.Replica = b.replica
		rb.Header.ID = uuid.NewV4().Bytes()
		rb.Requests = append(rb.Requests, req)
		b.batches = append(b.batches, newBatch(b.logger, rb, cb, tp, n))
	}
}
