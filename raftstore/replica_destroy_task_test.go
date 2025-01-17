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
	"context"
	"sync"
	"testing"
	"time"

	"github.com/matrixorigin/matrixcube/pb/metapb"
	"github.com/matrixorigin/matrixcube/util/leaktest"
	"github.com/stretchr/testify/assert"
)

type testDestroyMetadataStorage struct {
	sync.Mutex
	data     map[uint64]*metapb.DestroyingStatus
	c        chan struct{}
	watchPut bool
}

func newTestDestroyMetadataStorage(watchPut bool) *testDestroyMetadataStorage {
	return &testDestroyMetadataStorage{
		data:     make(map[uint64]*metapb.DestroyingStatus),
		c:        make(chan struct{}),
		watchPut: watchPut,
	}
}

func (s *testDestroyMetadataStorage) CreateDestroying(shardID uint64, index uint64, removeData bool, replicas []uint64) (metapb.ShardState, error) {
	s.Lock()
	defer s.Unlock()

	status := &metapb.DestroyingStatus{
		Index:      index,
		RemoveData: removeData,
		State:      metapb.ShardState_Destroying,
		Replicas:   make(map[uint64]bool),
	}
	for _, replicaID := range replicas {
		status.Replicas[replicaID] = false
	}

	s.data[shardID] = status
	if s.watchPut {
		s.c <- struct{}{}
	}
	return metapb.ShardState_Destroying, nil
}

func (s *testDestroyMetadataStorage) GetDestroying(shardID uint64) (*metapb.DestroyingStatus, error) {
	s.Lock()
	defer s.Unlock()

	return s.data[shardID], nil
}

func (s *testDestroyMetadataStorage) ReportDestroyed(shardID uint64, replicaID uint64) (metapb.ShardState, error) {
	s.Lock()
	defer s.Unlock()

	status, ok := s.data[shardID]
	if !ok {
		return metapb.ShardState_Destroying, nil
	}

	status.Replicas[replicaID] = true

	n := 0
	for _, v := range status.Replicas {
		if v {
			n++
		}
	}
	if n == len(status.Replicas) {
		status.State = metapb.ShardState_Destroyed
	}

	return status.State, nil
}

func TestDestroyTaskWithMultiTimes(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()
	pr := newTestReplica(Shard{ID: 1}, Replica{ID: 1}, s)
	pr.destroyTaskMu.hasTask = true
	pr.destroyTaskMu.reason = "1"

	pr.startDestroyReplicaTask(0, false, "2")
	assert.Equal(t, "1", pr.destroyTaskMu.reason)
}

func TestDestroyTaskWithStartCheckLogCommittedStep(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()
	pr := newTestReplica(Shard{ID: 1}, Replica{ID: 1}, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dms := newTestDestroyMetadataStorage(false)
	c := make(chan []uint64)
	f := newTestDestroyReplicaTaskFactory(false).setDestroyingStorage(dms).setActionHandler(func(a action) {
		if a.actionType == checkLogCommittedAction {
			assert.NotNil(t, a.actionCallback)
			c <- []uint64{1, 2, 3}
		}
	}).setCheckInterval(time.Millisecond * 10)
	go f.new(pr, 100, false, "TestDestroyTaskWithStartCheckLogCommittedStep").run(ctx)
	select {
	case <-c:
		break
	case <-time.After(time.Second * 100):
		assert.Fail(t, "timeout")
	}
}

func TestDestroyTaskWithCompleteCheckLogCommittedStep(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()
	pr := newTestReplica(Shard{ID: 1}, Replica{ID: 1}, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dms := newTestDestroyMetadataStorage(true)
	f := newTestDestroyReplicaTaskFactory(false).setDestroyingStorage(dms).setActionHandler(func(a action) {
		if a.actionType == checkLogCommittedAction {
			assert.NotNil(t, a.actionCallback)
			go a.actionCallback([]uint64{1, 2, 3})
		}
	}).setCheckInterval(time.Millisecond * 10)
	go f.new(pr, 100, false, "TestDestroyTaskWithCompleteCheckLogCommittedStep").run(ctx)

	select {
	case <-dms.c:
		v, err := dms.GetDestroying(pr.shardID)
		assert.NoError(t, err)
		assert.Equal(t, uint64(100), v.Index)
		assert.Equal(t, 3, len(v.Replicas))
	case <-time.After(time.Second * 100):
		assert.Fail(t, "timeout")
	}
}

func TestDestroyTaskWithStartCheckLogAppliedStep(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()
	pr := newTestReplica(Shard{ID: 1}, Replica{ID: 1}, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan struct{})
	dms := newTestDestroyMetadataStorage(false)
	dms.CreateDestroying(pr.shardID, 100, false, []uint64{1, 2, 3})

	f := newTestDestroyReplicaTaskFactory(false).setDestroyingStorage(dms).setActionHandler(func(a action) {
		if a.actionType == checkLogAppliedAction {
			assert.NotNil(t, a.actionCallback)
			c <- struct{}{}
		}
	}).setCheckInterval(time.Millisecond * 10)
	go f.new(pr, 100, false, "TestDestroyTaskWithStartCheckLogAppliedStep").run(ctx)

	select {
	case <-c:
		break
	case <-time.After(time.Second * 100):
		assert.Fail(t, "timeout")
	}
}

func TestDestroyTaskWithStartCompleteCheckLogAppliedStep(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()
	pr := newTestReplica(Shard{ID: 1}, Replica{ID: 1}, s)
	s.addReplica(pr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan struct{})
	dms := newTestDestroyMetadataStorage(false)
	dms.CreateDestroying(pr.shardID, 100, false, []uint64{1, 2, 3})

	f := newTestDestroyReplicaTaskFactory(false).setDestroyingStorage(dms).setActionHandler(func(a action) {
		if a.actionType == checkLogAppliedAction {
			go a.actionCallback(nil)
		}
	}).setCheckInterval(time.Millisecond * 10)
	go func() {
		f.new(pr, 100, false, "TestDestroyTaskWithStartCompleteCheckLogAppliedStep").run(ctx)
		close(c)
	}()
	select {
	case <-c:
		v, err := dms.GetDestroying(pr.shardID)
		assert.NoError(t, err)
		assert.Equal(t, uint64(100), v.Index)
		assert.True(t, v.Replicas[1])
	case <-time.After(time.Second * 100):
		assert.Fail(t, "timeout")
	}
}

type emptyTask struct {
}

func (t *emptyTask) run(ctx context.Context) {

}

type testDestroyReplicaTaskFactory struct {
	sync.RWMutex

	noop          bool
	actionHandler actionHandleFunc
	storage       destroyingStorage
	checkInterval time.Duration
}

func newTestDestroyReplicaTaskFactory(noop bool) *testDestroyReplicaTaskFactory {
	return &testDestroyReplicaTaskFactory{
		noop: noop,
	}
}

func (f *testDestroyReplicaTaskFactory) setActionHandler(actionHandler actionHandleFunc) *testDestroyReplicaTaskFactory {
	f.Lock()
	defer f.Unlock()

	f.actionHandler = actionHandler
	return f
}

func (f *testDestroyReplicaTaskFactory) setDestroyingStorage(storage destroyingStorage) *testDestroyReplicaTaskFactory {
	f.Lock()
	defer f.Unlock()

	f.storage = storage
	return f
}

func (f *testDestroyReplicaTaskFactory) setCheckInterval(checkInterval time.Duration) *testDestroyReplicaTaskFactory {
	f.Lock()
	defer f.Unlock()

	f.checkInterval = checkInterval
	return f
}

func (f *testDestroyReplicaTaskFactory) new(pr *replica, targetIndex uint64, removeData bool, reason string) destroyReplicaTask {
	f.RLock()
	defer f.RUnlock()

	if f.noop {
		return &emptyTask{}
	}

	actionHandler := f.actionHandler
	if actionHandler == nil {
		actionHandler = pr.addAction
	}

	storage := f.storage
	if storage == nil {
		storage = pr.store.Prophet().GetClient()
	}

	checkInterval := f.checkInterval
	if checkInterval == 0 {
		checkInterval = defaultCheckInterval
	}

	return newDefaultDestroyReplicaTaskFactory(actionHandler, storage, checkInterval).new(pr, targetIndex, removeData, reason)
}
