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
	"sync"
	"sync/atomic"

	"github.com/fagongzi/util/protoc"
	"github.com/lni/goutils/syncutil"
	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/components/prophet/event"
	"github.com/matrixorigin/matrixcube/pb/metapb"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/util"
	"go.uber.org/zap"
)

// Router route the request to the corresponding shard
type Router interface {
	// Start the router
	Start() error
	// Stop stops the router
	Stop()

	// SelectShardIDByKey Returns shardid where the key is located
	SelectShardIDByKey(group uint64, key []byte) uint64

	// AscendRange iterate through all shards in order within [Start, end), and stop when fn returns false.
	AscendRange(group uint64, start, end []byte, policy rpcpb.ReplicaSelectPolicy, fn func(shard Shard, replicaStore metapb.Store) bool)
	// SelectShardWithPolicy Select a Shard according to the specified Key, and select the Store where the
	// Shard's Replica is located according to the ReplicaSelectPolicy.
	SelectShardWithPolicy(group uint64, key []byte, policy rpcpb.ReplicaSelectPolicy) (Shard, metapb.Store)
	// SelectReplicaStoreWithPolicy select the Store where the shard's replica is located according to the
	// ReplicaSelectPolicy
	SelectReplicaStoreWithPolicy(shardID uint64, policy rpcpb.ReplicaSelectPolicy) metapb.Store

	// Deprecated: SelectShard returns a shard and leader store that the key is in the range [shard.Start, shard.End).
	// If returns leader address is "", means the current shard has no leader. Use `SelectShardWithPolicy` instead.
	SelectShard(group uint64, key []byte) (Shard, string)
	// Deprecated: Every do with all shards.  Use `AscendRange` instead.
	Every(group uint64, mustLeader bool, fn func(shard Shard, store metapb.Store) bool)
	// Deprecated: ForeachShards foreach shards
	ForeachShards(group uint64, fn func(shard Shard) bool)
	// GetShard returns the shard by shard id
	GetShard(id uint64) Shard

	// UpdateLeader update shard leader
	UpdateLeader(shardID uint64, leaderReplciaID uint64)
	// UpdateShard update shard metadata
	UpdateShard(shard Shard)
	// UpdateStore update store metadata
	UpdateStore(store metapb.Store)

	// Deprecated: LeaderReplicaStore return leader replica store. Use `SelectReplicaStoreWithPolicy` instead.
	LeaderReplicaStore(shardID uint64) metapb.Store
	// Deprecated: RandomReplicaStore return random replica store. Use `SelectReplicaStoreWithPolicy` instead.
	RandomReplicaStore(shardID uint64) metapb.Store

	// GetShardStats returns the runtime stats info of the shard
	GetShardStats(id uint64) metapb.ShardStats
	// GetStoreStats returns the runtime stats info of the store
	GetStoreStats(id uint64) metapb.StoreStats
}

type op struct {
	value uint64
}

func (o *op) next() uint64 {
	return atomic.AddUint64(&o.value, 1)
}

type routerOptions struct {
	logger             *zap.Logger
	fields             []zap.Field
	stopper            *syncutil.Stopper
	removeShardHandler func(id uint64)
	createShardHandler func(shard Shard)
}

func (opts *routerOptions) adjust() {
	opts.logger = log.Adjust(opts.logger)

	if opts.stopper == nil {
		opts.stopper = syncutil.NewStopper()
	}

	if opts.removeShardHandler == nil {
		opts.removeShardHandler = func(id uint64) {}
	}

	if opts.createShardHandler == nil {
		opts.createShardHandler = func(shard Shard) {}
	}
}

type routerBuilder struct {
	options *routerOptions
}

func newRouterBuilder() *routerBuilder {
	return &routerBuilder{
		options: &routerOptions{},
	}
}

func (rb *routerBuilder) withLogger(logger *zap.Logger, fields ...zap.Field) *routerBuilder {
	rb.options.logger = logger
	return rb
}

func (rb *routerBuilder) withStopper(stopper *syncutil.Stopper) *routerBuilder {
	rb.options.stopper = stopper
	return rb
}

func (rb *routerBuilder) withRemoveShardHandle(handle func(id uint64)) *routerBuilder {
	rb.options.removeShardHandler = handle
	return rb
}

func (rb *routerBuilder) withCreatShardHandle(handle func(shard Shard)) *routerBuilder {
	rb.options.createShardHandler = handle
	return rb
}

func (rb *routerBuilder) build(eventC chan rpcpb.EventNotify) (Router, error) {
	return newRouter(eventC, rb.options)
}

type defaultRouter struct {
	options *routerOptions
	logger  *zap.Logger
	eventC  chan rpcpb.EventNotify

	mu struct {
		sync.RWMutex

		keyRanges                map[uint64]*util.ShardTree   // shard.Group -> *util.ShardTree
		leaders                  map[uint64]metapb.Store      // shard id -> leader replica store
		stores                   map[uint64]metapb.Store      // store id -> metapb.Store metadata
		shards                   map[uint64]Shard             // shard id -> metapb.Shard
		missingLeaderStoreShards map[uint64]Replica           // shard id -> Replica
		opts                     map[uint64]op                // shard id -> op
		shardStats               map[uint64]metapb.ShardStats // shard id -> metapb.ShardStats
		storeStats               map[uint64]metapb.StoreStats // store id -> metapb.StoreStats
	}
}

func newRouter(eventC chan rpcpb.EventNotify, options *routerOptions) (Router, error) {
	options.adjust()
	r := &defaultRouter{
		options: options,
		logger:  options.logger.Named("router").With(options.fields...),
		eventC:  eventC,
	}
	r.mu.keyRanges = make(map[uint64]*util.ShardTree)
	r.mu.leaders = make(map[uint64]metapb.Store)
	r.mu.stores = make(map[uint64]metapb.Store)
	r.mu.shards = make(map[uint64]metapb.Shard)
	r.mu.missingLeaderStoreShards = make(map[uint64]Replica)
	r.mu.opts = make(map[uint64]op)
	r.mu.shardStats = make(map[uint64]metapb.ShardStats)
	r.mu.storeStats = make(map[uint64]metapb.StoreStats)
	return r, nil
}

func (r *defaultRouter) Start() error {
	r.options.stopper.RunWorker(r.eventLoop)
	return nil
}

func (r *defaultRouter) Stop() {
	r.options.stopper.Stop()
}

func (r *defaultRouter) SelectShardIDByKey(group uint64, key []byte) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.searchShardLocked(group, key).ID
}

func (r *defaultRouter) SelectShard(group uint64, key []byte) (Shard, string) {
	shard, store := r.SelectShardWithPolicy(group, key, rpcpb.SelectLeader)
	return shard, store.ClientAddress
}

func (r *defaultRouter) AscendRange(group uint64, start, end []byte,
	policy rpcpb.ReplicaSelectPolicy,
	fn func(shard Shard, replciaStore metapb.Store) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if tree, ok := r.mu.keyRanges[group]; ok {
		tree.AscendRange(start, end, func(shard *metapb.Shard) bool {
			s := *shard
			return fn(s, r.selectReplicaStoreByPolicyLocked(s, policy))
		})
	}
}

func (r *defaultRouter) SelectShardWithPolicy(group uint64, key []byte, policy rpcpb.ReplicaSelectPolicy) (Shard, metapb.Store) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	shard := r.searchShardLocked(group, key)
	return shard, r.selectReplicaStoreByPolicyLocked(shard, policy)
}

func (r *defaultRouter) SelectReplicaStoreWithPolicy(shardID uint64, policy rpcpb.ReplicaSelectPolicy) metapb.Store {
	r.mu.RLock()
	defer r.mu.RUnlock()

	shard, ok := r.mu.shards[shardID]
	if !ok {
		return metapb.Store{}
	}

	return r.selectReplicaStoreByPolicyLocked(shard, policy)
}

func (r *defaultRouter) selectReplicaStoreByPolicyLocked(shard Shard, policy rpcpb.ReplicaSelectPolicy) metapb.Store {
	switch policy {
	case rpcpb.SelectLeader:
		return r.getLeaderReplicaStoreLocked(shard.ID)
	case rpcpb.SelectRandom:
		return r.mustGetStoreLocked(r.selectStoreLocked(shard))
	default:
		panic("not yet implemented")
	}
}

func (r *defaultRouter) GetShard(id uint64) Shard {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.mu.shards[id]
}

func (r *defaultRouter) Every(group uint64, mustLeader bool, doFunc func(Shard, metapb.Store) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for id, shard := range r.mu.shards {
		if shard.Group == group {
			next := false
			if mustLeader {
				next = doFunc(shard, r.getLeaderReplicaStoreLocked(id))
			} else {
				storeID := r.selectStoreLocked(shard)
				next = doFunc(shard, r.mustGetStoreLocked(storeID))
			}
			if !next {
				return
			}
		}
	}
}

func (r *defaultRouter) ForeachShards(group uint64, fn func(shard Shard) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, shard := range r.mu.shards {
		if shard.Group == group {
			if !fn(shard) {
				return
			}
		}
	}
}

func (r *defaultRouter) LeaderReplicaStore(shardID uint64) metapb.Store {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.getLeaderReplicaStoreLocked(shardID)
}

func (r *defaultRouter) RandomReplicaStore(shardID uint64) metapb.Store {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if shard, ok := r.mu.shards[shardID]; ok {
		return r.mustGetStoreLocked(r.selectStoreLocked(shard))
	}

	return metapb.Store{}
}

func (r *defaultRouter) GetShardStats(id uint64) metapb.ShardStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.mu.shardStats[id]
}

func (r *defaultRouter) GetStoreStats(id uint64) metapb.StoreStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.mu.storeStats[id]
}

func (r *defaultRouter) UpdateLeader(shardID uint64, leaderReplciaID uint64) {
	if leaderReplciaID == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.updateLeaderLocked(shardID, leaderReplciaID)
}

func (r *defaultRouter) UpdateShard(shard Shard) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.updateShardLocked(protoc.MustMarshal(&shard), 0, false, false)
}

func (r *defaultRouter) UpdateStore(store metapb.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.updateStoreLocked(protoc.MustMarshal(&store))
}

func (r *defaultRouter) eventLoop() {
	r.logger.Info("router event loop task started")

	for {
		select {
		case <-r.options.stopper.ShouldStop():
			r.logger.Info("router event loop task stopped")
			return
		case evt := <-r.eventC:
			r.handleEvent(evt)
		}
	}
}

func (r *defaultRouter) handleEvent(evt rpcpb.EventNotify) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch evt.Type {
	case event.InitEvent:
		r.logger.Info("reset",
			zap.String("event", event.TypeName(evt.Type)),
			zap.Int("shard-count", len(evt.InitEvent.Shards)),
			zap.Int("store-count", len(evt.InitEvent.Stores)))
		for key := range r.mu.keyRanges {
			delete(r.mu.keyRanges, key)
		}

		for _, data := range evt.InitEvent.Stores {
			r.updateStoreLocked(data)
		}

		for i, data := range evt.InitEvent.Shards {
			r.updateShardLocked(data, evt.InitEvent.Leaders[i], false, false)
		}
	case event.ShardEvent:
		r.updateShardLocked(evt.ShardEvent.Data, evt.ShardEvent.Leader,
			evt.ShardEvent.Removed, evt.ShardEvent.Create)
	case event.StoreEvent:
		r.updateStoreLocked(evt.StoreEvent.Data)
	case event.ShardStatsEvent:
		r.mu.shardStats[evt.ShardStatsEvent.ShardID] = *evt.ShardStatsEvent
	case event.StoreStatsEvent:
		r.mu.storeStats[evt.StoreStatsEvent.StoreID] = *evt.StoreStatsEvent
	}
}

func (r *defaultRouter) updateShardLocked(data []byte, leaderReplicaID uint64, removed bool, create bool) {
	res := metapb.Shard{}
	err := res.Unmarshal(data)
	if err != nil {
		r.logger.Fatal("fail to unmarshal shard",
			zap.Error(err),
			log.HexField("data", data))
	}

	if removed {
		r.logger.Info("need to delete shard",
			log.ShardField("shard", res))

		r.options.removeShardHandler(res.GetID())
		if tree, ok := r.mu.keyRanges[res.GetGroup()]; ok {
			tree.Remove(res)
		}
		delete(r.mu.shards, res.GetID())
		delete(r.mu.missingLeaderStoreShards, res.GetID())
		delete(r.mu.leaders, res.GetID())
		return
	}

	if create {
		r.logger.Info("need to create shard",
			log.ShardField("shard", res))
		r.options.createShardHandler(res)
		return
	}

	r.mu.shards[res.GetID()] = res
	r.updateShardKeyRangeLocked(res)

	r.logger.Debug("shard route updated",
		log.ShardField("shard", res),
		zap.Uint64("leader", leaderReplicaID))

	if leaderReplicaID > 0 {
		r.updateLeaderLocked(res.GetID(), leaderReplicaID)
	}
}

func (r *defaultRouter) updateStoreLocked(data []byte) {
	s := metapb.NewStore()
	err := s.Unmarshal(data)
	if err != nil {
		r.logger.Fatal("fail to unmarshal store",
			zap.Error(err),
			log.HexField("data", data))
	}

	r.mu.stores[s.GetID()] = *s
	for k, v := range r.mu.missingLeaderStoreShards {
		if v.StoreID == s.GetID() {
			if _, ok := r.mu.shards[k]; ok {
				r.updateLeaderLocked(k, v.ID)
			}
		}
	}
}

func (r *defaultRouter) updateLeaderLocked(shardID, leaderReplicaID uint64) {
	shard := r.mustGetShardLocked(shardID)

	for _, p := range shard.Replicas {
		if p.ID == leaderReplicaID {
			if s, ok := r.mu.stores[p.StoreID]; ok {
				delete(r.mu.missingLeaderStoreShards, shardID)
				r.mu.leaders[shard.ID] = s
				r.logger.Info("shard leader updated",
					log.ShardIDField(shardID),
					log.ReplicaField("leader-replica", p),
					zap.String("address", s.ClientAddress))
				return
			}

			// wait store event
			r.mu.missingLeaderStoreShards[shardID] = p
			break
		}
	}

	r.logger.Info("skip shard leader",
		log.ShardIDField(shardID),
		log.ReasonField("missing store"))
}

func (r *defaultRouter) mustGetShardLocked(id uint64) Shard {
	value, ok := r.mu.shards[id]
	if !ok {
		r.logger.Fatal("shard must exist",
			log.ShardIDField(id))
	}

	return value
}

func (r *defaultRouter) updateShardKeyRangeLocked(shard Shard) {
	if shard.State == metapb.ShardState_Destroying ||
		shard.State == metapb.ShardState_Destroyed {
		return
	}

	if tree, ok := r.mu.keyRanges[shard.Group]; ok {
		tree.Update(shard)
		return
	}

	tree := util.NewShardTree()
	tree.Update(shard)

	r.mu.keyRanges[shard.Group] = tree
}

func (r *defaultRouter) mustGetStoreLocked(id uint64) metapb.Store {
	value, ok := r.mu.stores[id]
	if !ok {
		r.logger.Fatal("store must exist",
			log.StoreIDField(id))
	}

	return value
}

func (r *defaultRouter) getLeaderReplicaStoreLocked(shardID uint64) metapb.Store {
	if value, ok := r.mu.leaders[shardID]; ok {
		return value
	}
	r.logger.Debug("missing leader",
		log.ShardIDField(shardID))
	return metapb.Store{}
}

func (r *defaultRouter) getRandomReplicaStoreLocked(shardID uint64) metapb.Store {
	if value, ok := r.mu.leaders[shardID]; ok {
		return value
	}
	r.logger.Debug("missing leader",
		log.ShardIDField(shardID))
	return metapb.Store{}
}

func (r *defaultRouter) selectStoreLocked(shard Shard) uint64 {
	ops := r.mu.opts[shard.ID]
	storeID := shard.Replicas[int(ops.next())%len(shard.Replicas)].StoreID
	r.mu.opts[shard.ID] = ops
	return storeID
}

func (r *defaultRouter) searchShardLocked(group uint64, key []byte) Shard {
	if tree, ok := r.mu.keyRanges[group]; ok {
		return tree.Search(key)
	}
	r.logger.Debug("fail to search shard",
		zap.Uint64("group", group),
		log.HexField("key", key))
	return Shard{}
}

// NewMockRouter returns a mock router for testing.
func NewMockRouter() Router {
	r, _ := newRouterBuilder().build(make(chan rpcpb.EventNotify))
	return r
}
