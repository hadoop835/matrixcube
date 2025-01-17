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

package client

import (
	"context"
	"sync"

	"github.com/fagongzi/util/hack"
	"github.com/fagongzi/util/protoc"
	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/pb/metapb"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/pb/txnpb"
	"github.com/matrixorigin/matrixcube/raftstore"
	"github.com/matrixorigin/matrixcube/util/uuid"
	"go.uber.org/zap"
)

// Option client option
type Option func(*Future)

// WithShardGroup set shard group to execute the request
func WithShardGroup(group uint64) Option {
	return func(c *Future) {
		c.req.Group = group
	}
}

// WithRouteKey use the specified key to route request
func WithRouteKey(key []byte) Option {
	return func(c *Future) {
		c.req.Key = key
	}
}

// WithKeysRange If the current request operates on multiple Keys, set the range [from, to) of Keys
// operated by the current request. The client needs to split the request again if it wants
// to re-route according to KeysRange after the data management scope of the Shard has
// changed, or if it returns the specified error.
func WithKeysRange(from, to []byte) Option {
	return func(c *Future) {
		c.req.KeysRange = &rpcpb.Range{From: from, To: to}
	}
}

// WithShard use the specified shard to route request
func WithShard(shard uint64) Option {
	return func(c *Future) {
		c.req.ToShard = shard
	}
}

// WithReplicaSelectPolicy set the ReplicaSelectPolicy for request, default is SelectLeader
func WithReplicaSelectPolicy(policy rpcpb.ReplicaSelectPolicy) Option {
	return func(f *Future) {
		f.req.ReplicaSelectPolicy = policy
	}
}

// Future is used to obtain response data synchronously.
type Future struct {
	txnResponse txnpb.TxnBatchResponse
	value       []byte
	err         error
	req         rpcpb.Request
	ctx         context.Context
	c           chan struct{}

	mu struct {
		sync.Mutex
		closed bool
	}
}

func newFuture(ctx context.Context, req rpcpb.Request) *Future {
	return &Future{
		ctx: ctx,
		req: req,
		c:   make(chan struct{}, 1),
	}
}

// Get get the response data synchronously, blocking until `context.Done` or the response is received.
// This method cannot be called more than once. After calling `Get`, `Close` must be called to close
// `Future`.
func (f *Future) Get() ([]byte, error) {
	select {
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	case <-f.c:
		return f.value, f.err
	}
}

// GetTxn get the txn response data synchronously, blocking until `context.Done` or the response is received.
// This method cannot be called more than once. After calling `Get`, `Close` must be called to close
// `Future`.
func (f *Future) GetTxn() (txnpb.TxnBatchResponse, error) {
	select {
	case <-f.ctx.Done():
		return f.txnResponse, f.ctx.Err()
	case <-f.c:
		return f.txnResponse, f.err
	}
}

// Close close the future.
func (f *Future) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()

	close(f.c)
	f.mu.closed = true
}

func (f *Future) canRetry() bool {
	select {
	case <-f.ctx.Done():
		return false
	default:
		return true
	}
}

func (f *Future) done(value []byte, txnRespopnse *txnpb.TxnBatchResponse, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.mu.closed {
		if txnRespopnse != nil {
			f.txnResponse = *txnRespopnse
		}
		f.value = value
		f.err = err
		select {
		case f.c <- struct{}{}:
		default:
			panic("BUG")
		}
	}
}

// Client is a cube client, providing read and write access to the external.
type Client interface {
	// Start start the cube client
	Start() error
	// Stop stop the cube client
	Stop() error

	// Router returns a Router with real-time updated routing table information
	// inside for custom message routing
	Router() raftstore.Router

	// Admin exec the admin request, and use the `Future` to get the response.
	Admin(ctx context.Context, requestType uint64, payload []byte, opts ...Option) *Future
	// Write exec the write request, and use the `Future` to get the response.
	Write(ctx context.Context, requestType uint64, payload []byte, opts ...Option) *Future
	// Read exec the read request, and use the `Future` to get the response
	Read(ctx context.Context, requestType uint64, payload []byte, opts ...Option) *Future
	// Txn exec the transaction request, and use the `Future` to get the response
	Txn(ctx context.Context, request txnpb.TxnBatchRequest, opts ...Option) *Future

	// AddLabelToShard add lable to shard, and use the `Future` to get the response
	AddLabelToShard(ctx context.Context, name, value string, shard uint64) *Future
}

var _ Client = (*client)(nil)

// client a tcp application server
type client struct {
	logger      *zap.Logger
	shardsProxy raftstore.ShardsProxy
	inflights   sync.Map // request id -> *Future

}

// NewClient creates and return a cube client
func NewClient(cfg Cfg) Client {
	return NewClientWithOptions(CreateWithLogger(cfg.Store.GetConfig().Logger.Named("cube-client")),
		CreateWithShardsProxy(cfg.Store.GetShardsProxy()))
}

// NewClientWithOptions create client wiht options
func NewClientWithOptions(options ...CreateOption) Client {
	c := &client{}
	for _, opt := range options {
		opt(c)
	}
	c.adjust()
	return c
}

func (s *client) adjust() {
	if s.logger == nil {
		s.logger = log.Adjust(nil).Named("cube-client")
	}

	if s.shardsProxy == nil {
		s.logger.Fatal("ShardsProxy not set")
	}
}

func (s *client) Start() error {
	s.logger.Info("begin to start cube client")
	s.shardsProxy.SetCallback(s.done, s.doneError)
	s.shardsProxy.SetRetryController(s)
	s.logger.Info("cube client started")
	return nil
}

func (s *client) Stop() error {
	s.logger.Info("cube client stopped")
	return nil
}

func (s *client) Router() raftstore.Router {
	return s.shardsProxy.Router()
}

func (s *client) Write(ctx context.Context, requestType uint64, payload []byte, opts ...Option) *Future {
	return s.exec(ctx, requestType, payload, rpcpb.Write, nil, opts...)
}

func (s *client) Read(ctx context.Context, requestType uint64, payload []byte, opts ...Option) *Future {
	return s.exec(ctx, requestType, payload, rpcpb.Read, nil, opts...)
}

func (s *client) Admin(ctx context.Context, requestType uint64, payload []byte, opts ...Option) *Future {
	return s.exec(ctx, requestType, payload, rpcpb.Admin, nil, opts...)
}

func (s *client) Txn(ctx context.Context, request txnpb.TxnBatchRequest, opts ...Option) *Future {
	return s.exec(ctx, 0, nil, rpcpb.Txn, &request, opts...)
}

func (s *client) AddLabelToShard(ctx context.Context, name, value string, shard uint64) *Future {
	payload := protoc.MustMarshal(&rpcpb.UpdateLabelsRequest{
		Labels: []metapb.Label{{Key: name, Value: value}},
		Policy: rpcpb.Add,
	})
	return s.exec(ctx, uint64(rpcpb.AdminUpdateLabels), payload, rpcpb.Admin, nil, WithShard(shard))
}

func (s *client) exec(ctx context.Context, requestType uint64, payload []byte, cmdType rpcpb.CmdType, txnRequest *txnpb.TxnBatchRequest, opts ...Option) *Future {
	req := rpcpb.Request{}
	req.ID = uuid.NewV4().Bytes()
	req.Type = cmdType
	req.CustomType = requestType
	req.Cmd = payload
	req.TxnBatchRequest = txnRequest

	f := newFuture(ctx, req)
	for _, opt := range opts {
		opt(f)
	}
	s.inflights.Store(hack.SliceToString(f.req.ID), f)

	if len(req.Key) > 0 && req.ToShard > 0 {
		s.logger.Fatal("route with key and route with shard cannot be set at the same time")
	}
	if _, ok := ctx.Deadline(); !ok {
		s.logger.Fatal("cube client must use timeout context")
	}

	if ce := s.logger.Check(zap.DebugLevel, "begin to send request"); ce != nil {
		ce.Write(log.RequestIDField(req.ID))
	}

	if err := s.shardsProxy.Dispatch(f.req); err != nil {
		f.done(nil, nil, err)
	}
	return f
}

func (s *client) Retry(requestID []byte) (rpcpb.Request, bool) {
	id := hack.SliceToString(requestID)
	if c, ok := s.inflights.Load(id); ok {
		f := c.(*Future)
		if f.canRetry() {
			return f.req, true
		}
	}

	return rpcpb.Request{}, false
}

func (s *client) done(resp rpcpb.Response) {
	if ce := s.logger.Check(zap.DebugLevel, "response received"); ce != nil {
		ce.Write(log.RequestIDField(resp.ID))
	}

	id := hack.SliceToString(resp.ID)
	if c, ok := s.inflights.Load(hack.SliceToString(resp.ID)); ok {
		s.inflights.Delete(id)
		c.(*Future).done(resp.Value, resp.TxnBatchResponse, nil)
	} else {
		if ce := s.logger.Check(zap.DebugLevel, "response skipped"); ce != nil {
			ce.Write(log.RequestIDField(resp.ID), log.ReasonField("missing ctx"))
		}
	}
}

func (s *client) doneError(requestID []byte, err error) {
	if ce := s.logger.Check(zap.DebugLevel, "error response received"); ce != nil {
		ce.Write(log.RequestIDField(requestID), zap.Error(err))
	}

	id := hack.SliceToString(requestID)
	if c, ok := s.inflights.Load(id); ok {
		s.inflights.Delete(id)
		c.(*Future).done(nil, nil, err)
	}
}
