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
	"sync"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring/roaring64"
	"github.com/fagongzi/util/protoc"
	"github.com/lni/goutils/syncutil"
	"github.com/matrixorigin/matrixcube/aware"
	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/components/prophet"
	"github.com/matrixorigin/matrixcube/components/prophet/event"
	putil "github.com/matrixorigin/matrixcube/components/prophet/util"
	"github.com/matrixorigin/matrixcube/config"
	"github.com/matrixorigin/matrixcube/logdb"
	"github.com/matrixorigin/matrixcube/pb/errorpb"
	"github.com/matrixorigin/matrixcube/pb/metapb"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/storage"
	"github.com/matrixorigin/matrixcube/storage/kv/pebble"
	"github.com/matrixorigin/matrixcube/transport"
	"github.com/matrixorigin/matrixcube/util"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.uber.org/zap"
)

const (
	// DO NOT CHANGE
	snapshotDirName = "snapshots"
)

// Store manage a set of raft group
type Store interface {
	// Start the raft store
	Start()
	// Stop the raft store
	Stop()
	// GetConfig returns the config of the store
	GetConfig() *config.Config
	// Meta returns store meta
	Meta() metapb.Store
	// GetRouter returns a router
	GetRouter() Router
	// GetShardsProxy get shards proxy to dispatch requests
	GetShardsProxy() ShardsProxy
	// OnRequest receive a request, and call cb while the request is completed
	OnRequest(rpcpb.Request) error
	// OnRequestWithCB receive a request, and call cb while the request is completed
	OnRequestWithCB(req rpcpb.Request, cb func(resp rpcpb.ResponseBatch)) error
	// DataStorageByGroup returns a DataStorage of the shard group
	DataStorageByGroup(uint64) storage.DataStorage
	// MaybeLeader returns the shard replica maybe leader
	MaybeLeader(uint64) bool
	// MustAllocID returns an uint64 id, panic if it has an error
	MustAllocID() uint64
	// Prophet return current prophet instance
	Prophet() prophet.Prophet

	// CreateShardPool create resource pools, the resource pool will create shards,
	// and try to maintain the number of shards in the pool not less than the `capacity`
	// parameter. This is an idempotent operation.
	CreateShardPool(...metapb.ShardPoolJobMeta) (ShardsPool, error)
	// GetShardPool returns `ShardsPool`, nil if `CreateShardPool` not completed
	GetShardPool() ShardsPool
}

type store struct {
	cfg    *config.Config
	logger *zap.Logger

	sync.RWMutex
	meta       metapb.Store
	pd         prophet.Prophet
	bootOnce   sync.Once
	pdStartedC chan struct{}

	kvStorage             storage.KVStorage
	logdb                 logdb.LogDB
	trans                 transport.Trans
	shardsProxy           ShardsProxy
	router                Router
	splitChecker          *splitChecker
	watcher               prophet.EventWatcher
	vacuumCleaner         *vacuumCleaner
	createShardsProtector *createShardsProtector
	keyRanges             sync.Map // group id -> *util.ShardTree
	replicaRecords        sync.Map // replica id -> metapb.Replica
	replicas              sync.Map // shard id -> *replica
	droppedVoteMsgs       sync.Map // shard id -> raftpb.Message

	state    uint32
	stopOnce sync.Once

	aware   aware.ShardStateAware
	stopper *syncutil.Stopper
	// the worker pool used to drive all replicas
	workerPool *workerPool
	// shard pool processor
	shardPool       *dynamicShardsPool
	groupController *replicaGroupController

	storageStatsReader storageStatsReader

	mu struct {
		sync.RWMutex
		unavailableShards *roaring64.Bitmap
	}
}

// NewStore returns a raft store
func NewStore(cfg *config.Config) Store {
	cfg.Adjust()
	kv := pebble.CreateLogDBStorage(cfg.DataPath, cfg.FS, cfg.Logger)
	logger := cfg.Logger.Named("store").With(zap.String("store", cfg.Prophet.Name))
	s := &store{
		kvStorage:             kv,
		meta:                  metapb.Store{},
		cfg:                   cfg,
		logger:                logger,
		logdb:                 logdb.NewKVLogDB(kv, logger.Named("logdb")),
		stopper:               syncutil.NewStopper(),
		createShardsProtector: newCreateShardsProtector(),
		groupController:       newReplicaGroupController(),
	}

	s.vacuumCleaner = newVacuumCleaner(s.vacuum)
	// TODO: make maxWaitToChecker configurable
	s.splitChecker = newSplitChecker(4, &storeReplicaGetter{s},
		func(group uint64) storage.Feature {
			return s.cfg.Storage.DataStorageFactory(group).Feature()
		}, func(group uint64) splitCheckFunc {
			return s.cfg.Storage.DataStorageFactory(group).SplitCheck
		})
	s.workerPool = newWorkerPool(s.logger, s.logdb, &storeReplicaLoader{s}, s.cfg.Worker.RaftEventWorkers)
	s.shardPool = newDynamicShardsPool(cfg, s.logger)

	if s.cfg.Customize.CustomShardStateAwareFactory != nil {
		s.aware = cfg.Customize.CustomShardStateAwareFactory()
	}

	if s.cfg.UseMemoryAsStorage {
		s.storageStatsReader = newMemoryStorageStatsReader()
	} else {
		s.storageStatsReader = newDiskStorageStatsReader(s.cfg.DataPath)
	}

	s.mu.unavailableShards = roaring64.New()
	return s
}

func (s *store) GetConfig() *config.Config {
	return s.cfg
}

func (s *store) Start() {
	s.logger.Info("begin to start raftstore")
	s.workerPool.start()
	s.logger.Info("worker pool started",
		s.storeField())

	s.vacuumCleaner.start()
	s.logger.Info("vacuum cleaner started",
		s.storeField())

	s.splitChecker.start()
	s.logger.Info("split checker started",
		s.storeField())

	s.startProphet()
	s.logger.Info("prophet started",
		s.storeField())

	s.createTransport()
	s.logger.Info("raft internal transport created",
		s.storeField())

	s.startShards()
	s.logger.Info("shards started",
		s.storeField())

	s.startTransport()
	s.logger.Info("raft internal transport started",
		s.storeField(),
		log.ListenAddressField(s.cfg.RaftAddr))

	s.startTimerTasks()
	s.logger.Info("shard timer based tasks started",
		s.storeField())

	s.startRouter()
	s.logger.Info("router started",
		s.storeField())

	s.startShardsProxy()
	s.logger.Info("proxy started",
		s.storeField(),
		log.ListenAddressField(s.cfg.ClientAddr))

	s.handleStoreHeartbeatTask(time.Now())
}

func (s *store) Stop() {
	atomic.StoreUint32(&s.state, 1)

	s.stopOnce.Do(func() {
		s.logger.Info("begin to stop raftstore",
			s.storeField())

		s.splitChecker.close()
		s.logger.Info("split checker closed",
			s.storeField())

		s.pd.Stop()
		s.logger.Info("pd stopped",
			s.storeField())

		// vacuumCleaner must be closed when workerPool is still running
		s.vacuumCleaner.close()
		s.logger.Info("vacuum cleaner closed",
			s.storeField())

		s.trans.Close()
		s.logger.Info("raft internal transport stopped",
			s.storeField())

		// stop the router first to prevent any new replica to be created when
		// closing replicas.
		s.router.Stop()
		s.logger.Info("store router stopped",
			s.storeField())

		// requests all replicas to be shutdown
		s.forEachReplica(func(pr *replica) bool {
			pr.close()
			return true
		})
		s.logger.Info("shards requested to be stopped",
			s.storeField())
		// stop the worker pool
		s.workerPool.close()
		s.logger.Info("worker pool stopped",
			s.storeField())
		// worker pool stopped, it's now safe to check whether all replicas have been
		// shutdown, shutdown the replica if it is not stopped.
		s.forEachReplica(func(pr *replica) bool {
			if !pr.unloaded() {
				pr.shutdown()
			}
			return true
		})
		s.logger.Info("shards stopped",
			s.storeField())

		s.stopper.Stop()
		s.logger.Info("stopper stopped",
			s.storeField())

		s.shardsProxy.Stop()
		s.logger.Info("proxy stopped",
			s.storeField())

		s.kvStorage.Close()
		s.logger.Info("kvStorage closed")
	})
}

func (s *store) GetReplicaSnapshotDir(shardID uint64, replicaID uint64) string {
	dir := fmt.Sprintf("shard-%d-replica-%d", shardID, replicaID)
	return s.cfg.FS.PathJoin(s.cfg.DataPath, snapshotDirName, dir)
}

func (s *store) GetShardsProxy() ShardsProxy {
	return s.shardsProxy
}

func (s *store) GetRouter() Router {
	return s.router
}

func (s *store) startRouter() {
	watcher, err := s.pd.GetClient().NewWatcher(event.AllEvent)
	if err != nil {
		s.logger.Fatal("fail to create router",
			s.storeField(),
			zap.Error(err))
	}
	r, err := newRouterBuilder().
		withLogger(s.logger).
		withCreatShardHandle(func(shard Shard) {
			s.doDynamicallyCreate(shard)
		}).
		withRemoveShardHandle(func(id uint64) {
			s.destroyReplica(id, true, true, "remove by event")
		}).
		build(watcher.GetNotify())
	if err != nil {
		s.logger.Fatal("fail to create router",
			s.storeField(),
			zap.Error(err))
	}
	err = r.Start()
	if err != nil {
		s.logger.Fatal("fail to start router",
			s.storeField(),
			zap.Error(err))
	}

	s.router = r
	s.watcher = watcher
}

func (s *store) Meta() metapb.Store {
	return s.meta
}

func (s *store) OnRequest(req rpcpb.Request) error {
	if s.cfg.Customize.CustomShardProxyRequestHandler == nil {
		return s.OnRequestWithCB(req, s.shardsProxy.OnResponse)
	}

	handled, err := s.cfg.Customize.CustomShardProxyRequestHandler(req, s.shardsProxy.OnResponse)
	if err != nil {
		return err
	}

	if handled {
		return nil
	}
	return s.OnRequestWithCB(req, s.shardsProxy.OnResponse)
}

func (s *store) OnRequestWithCB(req rpcpb.Request, cb func(resp rpcpb.ResponseBatch)) error {
	if ce := s.logger.Check(zap.DebugLevel, "receive request"); ce != nil {
		ce.Write(log.RequestIDField(req.ID),
			s.storeField())
	}

	var pr *replica
	var err error
	if req.ToShard > 0 {
		pr = s.getReplica(req.ToShard, false)
		if pr == nil {
			if ce := s.logger.Check(zap.DebugLevel, "fail to handle request"); ce != nil {
				ce.Write(log.RequestIDField(req.ID),
					s.storeField(),
					log.ShardIDField(req.ToShard),
					log.ReasonField("shard not found"))
			}

			if s.isShardUnavailable(req.ToShard) {
				respShardUnavailable(req.ToShard, req, cb)
				return nil
			}

			respStoreNotMatch(errStoreNotMatch, req, cb)
			return nil
		}
	} else {
		pr, err = s.selectShard(req.Group, req.Key)
		if err != nil {
			if ce := s.logger.Check(zap.DebugLevel, "fail to handle request"); ce != nil {
				ce.Write(log.RequestIDField(req.ID),
					s.storeField(),
					log.HexField("key", req.Key),
					log.ReasonField("key not match"))
			}

			if err == errStoreNotMatch {
				respStoreNotMatch(err, req, cb)
				return nil
			}

			return err
		}
	}

	if err := pr.onReq(req, cb); err != nil {
		if s.isShardUnavailable(pr.getShardID()) {
			respShardUnavailable(pr.getShardID(), req, cb)
		} else {
			respStoreNotMatch(errStoreNotMatch, req, cb)
		}
	}
	return nil
}

func (s *store) DataStorageByGroup(group uint64) storage.DataStorage {
	return s.cfg.Storage.DataStorageFactory(group)
}

func (s *store) MaybeLeader(shard uint64) bool {
	return nil != s.getReplica(shard, true)
}

func (s *store) MustAllocID() uint64 {
	for {
		id, err := s.pd.GetClient().AllocID()
		if err == nil {
			return id
		}

		s.logger.Error("failed to alloc id",
			s.storeField(),
			zap.Error(err))
		time.Sleep(time.Millisecond * 200)
	}
}

func (s *store) Prophet() prophet.Prophet {
	return s.pd
}

func (s *store) startProphet() {
	s.cfg.Prophet.Handler = s
	s.cfg.Prophet.Adjust(nil, false)

	s.pdStartedC = make(chan struct{})
	s.pd = prophet.NewProphet(s.cfg)
	s.pd.Start()
	<-s.pdStartedC
	s.shardPool.setProphetClient(s.pd.GetClient())
}

func (s *store) createTransport() {
	s.trans = transport.NewTransport(s.logger,
		s.cfg.RaftAddr, s.Meta().ID, s.handle, s.unreachable, s.snapshotStatus,
		s.GetReplicaSnapshotDir, s.containerResolver, s.cfg.FS)
	if s.cfg.Customize.CustomWrapNewTransport != nil {
		s.trans = s.cfg.Customize.CustomWrapNewTransport(s.trans)
	}
	if s.cfg.Customize.CustomTransportFilter != nil {
		s.trans.SetFilter(s.cfg.Customize.CustomTransportFilter)
	}
}

func (s *store) startTransport() {
	s.trans.Start()
}

func (s *store) startShards() {
	totalCount := 0
	tombstoneCount := 0

	var tombstones []metapb.ShardLocalState
	shards := make(map[uint64]metapb.ShardLocalState)
	localDestroyings := make(map[uint64]metapb.ShardMetadata)
	confirmShards := roaring64.New()
	s.cfg.Storage.ForeachDataStorageFunc(func(group uint64, ds storage.DataStorage) {
		initStates, err := ds.GetInitialStates()
		if err != nil {
			s.logger.Fatal("fail to get initial state",
				s.storeField(),
				zap.Error(err))
		}

		for _, metadata := range initStates {
			totalCount++
			sls := metadata.Metadata
			if sls.Shard.ID != metadata.ShardID {
				s.logger.Fatal("BUG: shard id not match in metadata",
					s.storeField(),
					zap.Uint64("expect", sls.Shard.ID),
					zap.Uint64("actual", metadata.ShardID))
			}

			if sls.State == metapb.ReplicaState_ReplicaTombstone {
				tombstones = append(tombstones, sls)
				tombstoneCount++

				if sls.Shard.State == metapb.ShardState_Destroyed {
					s.createShardsProtector.addDestroyed(sls.Shard.ID)
				}

				s.logger.Info("shard is tombstone in store",
					s.storeField(),
					log.ShardIDField(sls.Shard.ID))
				continue
			}

			if metadata.Metadata.Shard.State == metapb.ShardState_Destroying {
				s.createShardsProtector.addDestroyed(sls.Shard.ID)
				localDestroyings[metadata.ShardID] = metadata
			} else {
				confirmShards.Add(sls.Shard.ID)
			}

			shards[sls.Shard.ID] = sls
		}
	})

	for {
		rsp, err := s.pd.GetClient().CheckShardState(confirmShards)
		if err != nil {
			s.logger.Error("failed to check init shards, retry later",
				zap.Error(err))
			continue
		}

		bm := putil.MustUnmarshalBM64(rsp.Destroyed)
		bm.Or(putil.MustUnmarshalBM64(rsp.Destroying))
		if bm.GetCardinality() > 0 {
			for _, id := range bm.ToArray() {
				s.createShardsProtector.addDestroyed(id)
				tombstones = append(tombstones, shards[id])
				delete(shards, id)
			}
		}
		break
	}

	var readyBootstrapShards []Shard
	for _, sls := range shards {
		readyBootstrapShards = append(readyBootstrapShards, sls.Shard)
	}

	newReplicaCreator(s).
		withReason("restart").
		withStartReplica(true, nil, func(r *replica) {
			if metadata, ok := localDestroyings[r.shardID]; ok {
				r.startDestroyReplicaTask(metadata.LogIndex, metadata.Metadata.RemoveData, "restart")
			}
		}).
		create(readyBootstrapShards)

	s.cleanupTombstones(tombstones)

	s.logger.Info("shards started",
		s.storeField(),
		zap.Int("total", totalCount),
		zap.Int("bootstrap", len(readyBootstrapShards)),
		zap.Int("tombstone", tombstoneCount))
}

func (s *store) addReplica(pr *replica) bool {
	_, loaded := s.replicas.LoadOrStore(pr.shardID, pr)
	return !loaded
}

func (s *store) removeReplica(shard Shard) {
	s.replicas.Delete(shard.ID)
	if s.aware != nil {
		s.aware.Destroyed(shard)
	}
}

func (s *store) startShardsProxy() {
	maxBodySize := int(s.cfg.Raft.MaxEntryBytes) * 2
	rpc := newProxyRPC(s.logger.Named("proxy.rpc").With(s.storeField()),
		s.cfg.ClientAddr,
		maxBodySize,
		s.OnRequest)

	l := s.logger.Named("proxy").With(s.storeField())
	sp, err := newShardsProxyBuilder().
		withLogger(l).
		withBackendFactory(newBackendFactory(l, s)).
		withMaxBodySize(maxBodySize).
		withRPC(rpc).
		build(s.router)
	if err != nil {
		s.logger.Fatal("fail to create shards proxy", zap.Error(err))
	}

	s.shardsProxy = sp
	err = s.shardsProxy.Start()
	if err != nil {
		s.logger.Fatal("fail to start shards proxy",
			s.storeField(),
			log.ListenAddressField(s.cfg.ClientAddr),
			zap.Error(err))
	}
}

func (s *store) getReplicaRecord(id uint64) (Replica, bool) {
	value, ok := s.replicaRecords.Load(id)
	if !ok {
		return Replica{}, false
	}

	return value.(Replica), true
}

func (s *store) forEachReplica(consumerFunc func(*replica) bool) {
	s.replicas.Range(func(key, value interface{}) bool {
		return consumerFunc(value.(*replica))
	})
}

func (s *store) getReplica(id uint64, mustLeader bool) *replica {
	if value, ok := s.replicas.Load(id); ok {
		pr := value.(*replica)
		if mustLeader && !pr.isLeader() {
			return nil
		}

		return pr
	}

	return nil
}

// In some case, the vote raft msg maybe dropped, so follower node can't respond the vote msg
// shard a has 3 replicas p1, p2, p3. The p1 split to new shard b
// case 1: in most case, p1 apply split raft log is before p2 and p3.
//         At this time, if p2, p3 received the shard b's vote msg,
//         and this vote will be dropped by p2 and p3 node,
//         because shard a and shard b has overlapped range at p2 and p3 node
// case 2: p2 or p3 apply split log is before p1, we can't mock shard b's vote msg
func (s *store) cacheDroppedVoteMsg(id uint64, msg metapb.RaftMessage) {
	if msg.Message.Type == raftpb.MsgVote ||
		msg.Message.Type == raftpb.MsgPreVote {
		s.droppedVoteMsgs.Store(id, msg)
	}
}

func (s *store) removeDroppedVoteMsg(id uint64) (metapb.RaftMessage, bool) {
	if value, ok := s.droppedVoteMsgs.Load(id); ok {
		s.droppedVoteMsgs.Delete(id)
		return value.(metapb.RaftMessage), true
	}

	return metapb.RaftMessage{}, false
}

func (s *store) validateStoreID(req rpcpb.RequestBatch) error {
	if req.Header.Replica.StoreID != s.meta.GetID() {
		return fmt.Errorf("store not match, give=<%d> want=<%d>",
			req.Header.Replica.StoreID,
			s.meta.GetID())
	}

	return nil
}

func (s *store) validateShard(req rpcpb.RequestBatch) (errorpb.Error, bool) {
	shardID := req.Header.ShardID
	replicaID := req.Header.Replica.ID

	pr := s.getReplica(shardID, false)
	if nil == pr {
		err := new(errorpb.ShardNotFound)
		err.ShardID = shardID
		return errorpb.Error{
			Message:       errShardNotFound.Error(),
			ShardNotFound: err,
		}, true
	}

	if !pr.isLeader() {
		err := new(errorpb.NotLeader)
		err.ShardID = shardID
		err.Leader, _ = s.getReplicaRecord(pr.getLeaderReplicaID())

		return errorpb.Error{
			Message:   errNotLeader.Error(),
			NotLeader: err,
		}, true
	}

	if pr.replicaID != replicaID {
		return errorpb.Error{
			Message: fmt.Sprintf("mismatch replica id, want %d, but %d", pr.replicaID, replicaID),
		}, true
	}

	shard := pr.getShard()
	if !checkEpoch(shard, req) {
		err := new(errorpb.StaleEpoch)
		// Attach the next shard which might be split from the current shard. But it doesn't
		// matter if the next shard is not split from the current shard. If the shard meta
		// received by the KV driver is newer than the meta cached in the driver, the meta is
		// updated.
		newShard := s.nextShard(shard)
		if newShard != nil {
			err.NewShards = append(err.NewShards, *newShard)
		}

		return errorpb.Error{
			Message:    errStaleEpoch.Error(),
			StaleEpoch: err,
		}, true
	}

	return errorpb.Error{}, false
}

func checkEpoch(shard Shard, req rpcpb.RequestBatch) bool {
	checkVer := false
	checkConfVer := false

	if req.IsAdmin() {
		switch req.GetAdminCmdType() {
		case rpcpb.AdminBatchSplit:
			checkVer = true
		case rpcpb.AdminConfigChange:
			checkConfVer = true
		case rpcpb.AdminTransferLeader:
			checkVer = true
			checkConfVer = true
		}
	} else {
		// for normal command, we don't care conf version.
		checkVer = true
	}

	if !checkConfVer && !checkVer {
		return true
	}

	if req.Header.IsEmpty() {
		return false
	}

	latestEpoch := shard.Epoch
	isStale := func(fromEpoch Epoch) bool {
		return (checkConfVer && fromEpoch.ConfigVer < latestEpoch.ConfigVer) ||
			(checkVer && fromEpoch.Generation < latestEpoch.Generation)
	}

	// only check first request, because requests inside a batch have the same epoch
	return req.Requests[0].IgnoreEpochCheck ||
		!isStale(req.Requests[0].Epoch)
}

func newAdminResponseBatch(adminType rpcpb.AdminCmdType, rsp protoc.PB) rpcpb.ResponseBatch {
	return rpcpb.ResponseBatch{
		Responses: []rpcpb.Response{
			{
				Value: protoc.MustMarshal(rsp),
			},
		},
	}
}

func (s *store) updateShardKeyRange(group uint64, shards ...Shard) {
	if value, ok := s.keyRanges.Load(group); ok {
		value.(*util.ShardTree).Update(shards...)
		return
	}

	tree := util.NewShardTree()
	tree.Update(shards...)

	value, loaded := s.keyRanges.LoadOrStore(group, tree)
	if loaded {
		value.(*util.ShardTree).Update(shards...)
	}
}

func (s *store) removeShardKeyRange(shard Shard) bool {
	if value, ok := s.keyRanges.Load(shard.Group); ok {
		return value.(*util.ShardTree).Remove(shard)
	}

	return false
}

func (s *store) selectShard(group uint64, key []byte) (*replica, error) {
	shard := s.searchShard(group, key)
	if shard.ID == 0 {
		return nil, errStoreNotMatch
	}

	pr, ok := s.replicas.Load(shard.ID)
	if !ok {
		return nil, errStoreNotMatch
	}

	return pr.(*replica), nil
}

func (s *store) searchShard(group uint64, key []byte) Shard {
	if value, ok := s.keyRanges.Load(group); ok {
		return value.(*util.ShardTree).Search(key)
	}

	return Shard{}
}

func (s *store) nextShard(shard Shard) *Shard {
	if value, ok := s.keyRanges.Load(shard.Group); ok {
		return value.(*util.ShardTree).NextShard(shard.Start)
	}

	return nil
}

func (s *store) storeField() zap.Field {
	s.RLock()
	defer s.RUnlock()

	return log.StoreIDField(s.meta.GetID())
}

func (s *store) containerResolver(storeID uint64) (string, error) {
	container, err := s.pd.GetStorage().GetStore(storeID)
	if err != nil {
		return "", err
	}
	if container == nil {
		return "", nil
	}
	return container.GetRaftAddress(), nil
}

func (s *store) unreachable(shardID uint64, replicaID uint64) {
	if pr := s.getReplica(shardID, true); pr != nil {
		pr.addFeedback(replicaID)
	}
}

func (s *store) snapshotStatus(shardID uint64,
	replicaID uint64, ss raftpb.Snapshot, rejected bool) {
	waitTime := 5 * time.Second
	if rejected {
		waitTime = 0 * time.Second
	}
	// when not rejected, we wait a few seconds before notifying the leader,
	// this prevents the leader sending a new append message only to be rejected
	// by the remote replica and triggering a new snapshot.
	s.stopper.RunWorker(func() {
		timer := time.NewTimer(waitTime)
		defer timer.Stop()
		select {
		case <-timer.C:
			if pr := s.getReplica(shardID, true); pr != nil {
				pr.addSnapshotStatus(snapshotStatus{to: replicaID, rejected: rejected})
				pr.removeSnapshot(ss, false)
			}
		case <-s.stopper.ShouldStop():
			return
		}
	})
}

func (s *store) getReplicaCount() uint64 {
	n := uint64(0)
	s.replicas.Range(func(key, value interface{}) bool {
		n++
		return true
	})
	return n
}

func (s *store) isShardUnavailable(id uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.mu.unavailableShards.Contains(id)
}

func (s *store) addUnavailableShard(id uint64) {
	s.mu.Lock()
	s.mu.unavailableShards.Add(id)
	s.mu.Unlock()
}

func (s *store) addUnavailableShardWithIds(shards *roaring64.Bitmap) {
	if shards.GetCardinality() == 0 {
		return
	}

	s.mu.Lock()
	s.mu.unavailableShards.Or(shards)
	s.mu.Unlock()
}

type storeReplicaGetter struct {
	store *store
}

func (s *storeReplicaGetter) getReplica(shardID uint64) (*replica, bool) {
	if r := s.store.getReplica(shardID, false); r != nil {
		return r, true
	}
	return nil, false
}

func (s *store) getStoreHeartbeat(last time.Time) (rpcpb.StoreHeartbeatReq, error) {
	stats := metapb.StoreStats{}
	stats.StoreID = s.Meta().ID

	v, err := s.storageStatsReader.stats()
	if err != nil {
		s.logger.Error("fail to get storage capacity status",
			s.storeField(),
			zap.Error(err))
		return rpcpb.StoreHeartbeatReq{}, err
	}
	stats.Capacity = v.capacity
	stats.UsedSize = v.usedSize
	stats.Available = v.available

	if s.cfg.Capacity > 0 {
		stats.Capacity = uint64(s.cfg.Capacity)
		// If `Capacity` set, calculate `Available` using `Capacity`
		stats.Available = stats.Capacity - stats.UsedSize
	}

	// cpu usages
	usages, err := util.CpuUsages()
	if err != nil {
		s.logger.Error("fail to get cpu status",
			s.storeField(),
			zap.Error(err))
		return rpcpb.StoreHeartbeatReq{}, err
	}
	for i, v := range usages {
		stats.CpuUsages = append(stats.CpuUsages, metapb.RecordPair{
			Key:   fmt.Sprintf("cpu:%d", i),
			Value: uint64(v * 100),
		})
	}

	// io rates
	rates, err := util.IORates(s.cfg.DataPath)
	if err != nil {
		s.logger.Error("fail to get io status",
			s.storeField(),
			zap.Error(err))
		return rpcpb.StoreHeartbeatReq{}, err
	}
	for name, v := range rates {
		stats.WriteIORates = append(stats.WriteIORates, metapb.RecordPair{
			Key:   name,
			Value: v.WriteBytes,
		})
		stats.ReadIORates = append(stats.ReadIORates, metapb.RecordPair{
			Key:   name,
			Value: v.ReadBytes,
		})
	}

	s.forEachReplica(func(pr *replica) bool {
		// TODO: re-enable this
		//if pr.ps.isApplyingSnapshot() {
		//	stats.ApplyingSnapCount++
		//}

		stats.ShardCount++
		return true
	})
	// FIXME: provide this count from the new implementation
	// stats.ReceivingSnapCount = s.snapshotManager.ReceiveSnapCount()
	stats.SendingSnapCount = s.trans.SendingSnapshotCount()
	stats.StartTime = uint64(s.Meta().StartTime)

	s.cfg.Storage.ForeachDataStorageFunc(func(_ uint64, db storage.DataStorage) {
		st := db.Stats()
		stats.WrittenBytes += st.WrittenBytes
		stats.WrittenKeys += st.WrittenKeys
		stats.ReadKeys += st.ReadKeys
		stats.ReadBytes += st.ReadBytes
	})

	// TODO: is busy
	stats.IsBusy = false
	stats.Interval = &metapb.TimeInterval{
		Start: uint64(last.Unix()),
		End:   uint64(time.Now().Unix()),
	}

	var data []byte
	if s.cfg.Customize.CustomStoreHeartbeatDataProcessor != nil {
		data = s.cfg.Customize.CustomStoreHeartbeatDataProcessor.CollectData()
	}
	return rpcpb.StoreHeartbeatReq{Stats: stats, Data: data}, nil
}

func (s *store) startHandleShardHeartbeat() {
	c, err := s.pd.GetClient().GetShardHeartbeatRspNotifier()
	if err != nil {
		s.logger.Fatal("tail to start handle resource heartbeat resp task",
			s.storeField(),
			zap.Error(err))
	}
	s.stopper.RunWorker(func() {
		for {
			select {
			case <-s.stopper.ShouldStop():
				s.logger.Info("handle resource heartbeat resp task stopped",
					s.storeField())
				return
			case rsp, ok := <-c:
				if ok {
					s.doShardHeartbeatRsp(rsp)
				}
			}
		}
	})
}

func (s *store) doShardHeartbeatRsp(rsp rpcpb.ShardHeartbeatRsp) {
	if rsp.DestroyDirectly {
		s.destroyReplica(rsp.ShardID, true, true, "remove by pd")
		return
	}

	pr := s.getReplica(rsp.ShardID, true)
	if pr == nil {
		s.logger.Info("skip heartbeat resp",
			s.storeField(),
			log.ShardIDField(rsp.ShardID),
			log.ReasonField("not leader"))
		return
	}

	if rsp.ConfigChange != nil {
		s.logger.Info("send conf change request",
			s.storeField(),
			log.ShardIDField(rsp.ShardID),
			log.ConfigChangeFieldWithHeartbeatResp("change", rsp))
		pr.addAdminRequest(rpcpb.AdminConfigChange, &rpcpb.ConfigChangeRequest{
			ChangeType: rsp.ConfigChange.ChangeType,
			Replica:    rsp.ConfigChange.Replica,
		})
	} else if rsp.ConfigChangeV2 != nil {
		s.logger.Info("send conf change request",
			s.storeField(),
			log.ShardIDField(rsp.ShardID),
			log.ConfigChangesFieldWithHeartbeatResp("changes", rsp))
		panic("ConfigChangeV2 request from prophet")
	} else if rsp.TransferLeader != nil {
		s.logger.Info("send transfer leader request",
			s.storeField(),
			log.ShardIDField(rsp.ShardID))
		pr.addAdminRequest(rpcpb.AdminTransferLeader, &rpcpb.TransferLeaderRequest{
			Replica: rsp.TransferLeader.Replica,
		})
	} else if rsp.SplitShard != nil {
		// currently, pd only support use keys to splits
		switch rsp.SplitShard.Policy {
		case metapb.CheckPolicy_USEKEY:
			shard := pr.getShard()
			splitIDs, err := pr.store.pd.GetClient().AskBatchSplit(shard, uint32(len(rsp.SplitShard.Keys)))
			if err != nil {
				s.logger.Error("fail to ask batch split",
					s.storeField(),
					log.ShardIDField(rsp.ShardID),
					zap.Error(err))
				return
			}
			pr.addAction(action{
				epoch:      rsp.ShardEpoch,
				actionType: splitAction,
				splitCheckData: splitCheckData{
					splitKeys: rsp.SplitShard.Keys,
					splitIDs:  splitIDs,
				},
			})
		}
	}
}

type storageStatsReader interface {
	stats() (storageStats, error)
}

type storageStats struct {
	capacity  uint64
	available uint64
	usedSize  uint64
}

type memoryStorageStatsReader struct {
}

func newMemoryStorageStatsReader() storageStatsReader {
	return &memoryStorageStatsReader{}
}

func (s *memoryStorageStatsReader) stats() (storageStats, error) {
	ms, err := util.MemStats()
	if err != nil {
		return storageStats{}, err
	}

	return storageStats{
		capacity:  ms.Total,
		usedSize:  ms.Total - ms.Available,
		available: ms.Available,
	}, nil
}

type diskStorageStatsReader struct {
	dir string
}

func newDiskStorageStatsReader(dir string) storageStatsReader {
	return &diskStorageStatsReader{dir: dir}
}

func (s *diskStorageStatsReader) stats() (storageStats, error) {
	ms, err := util.DiskStats(s.dir)
	if err != nil {
		return storageStats{}, err
	}

	return storageStats{
		capacity:  ms.Total,
		usedSize:  ms.Total - ms.Free,
		available: ms.Free,
	}, nil
}
