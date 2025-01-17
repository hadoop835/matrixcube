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
	"time"

	"github.com/fagongzi/util/protoc"
	"github.com/golang/mock/gomock"
	"github.com/matrixorigin/matrixcube/components/prophet/core"
	"github.com/matrixorigin/matrixcube/components/prophet/mock/mockclient"
	"github.com/matrixorigin/matrixcube/components/prophet/mock/mockjob"
	"github.com/matrixorigin/matrixcube/components/prophet/storage"
	"github.com/matrixorigin/matrixcube/pb/metapb"
	"github.com/matrixorigin/matrixcube/util/leaktest"
	"github.com/stretchr/testify/assert"
)

func TestNewDynamicShardsPool(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)
	assert.Equal(t, p, cfg.Prophet.GetJobProcessor(metapb.JobType_CreateShardPool))
}

func TestSetProphetClient(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)

	p.setProphetClient(nil)
	c := make(chan struct{})
	go func() {
		p.waitProphetClientSetted()
		c <- struct{}{}
	}()
	select {
	case <-c:
	case <-time.After(time.Second):
		assert.Fail(t, "fail")
	}
}

func TestAlloc(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	c := 0
	client := mockclient.NewMockClient(ctrl)
	client.EXPECT().ExecuteJob(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(func(metapb.Job, []byte) ([]byte, error) {
		defer func() {
			c++
		}()
		if c == 0 {
			return nil, errors.New("err")
		} else if c == 1 {
			return nil, nil
		}

		return protoc.MustMarshal(&metapb.AllocatedShard{ShardID: 1}), nil
	})

	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)
	p.setProphetClient(client)

	v, err := p.Alloc(0, nil)
	assert.Error(t, err)
	assert.Equal(t, metapb.AllocatedShard{}, v)
	assert.Equal(t, 2, c)

	v, err = p.Alloc(0, nil)
	assert.NoError(t, err)
	assert.Equal(t, metapb.AllocatedShard{ShardID: 1}, v)
	assert.Equal(t, 3, c)
}

func TestUnique(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)
	p.job = metapb.Job{Type: metapb.JobType_CreateShardPool}
	assert.Equal(t, "1-0-1", p.unique(0, 1))
}

func TestAddPrefix(t *testing.T) {
	defer leaktest.AfterTest(t)()

	assert.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0, 1}, addPrefix(nil, 1))
	assert.Equal(t, []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, addPrefix([]byte{1}, 1))
}

func TestGCAllocating(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)
	p.job = metapb.Job{Type: metapb.JobType_CreateShardPool}

	ss := storage.NewTestStorage()
	p.gcAllocating(ss, nil)
	v, err := ss.GetJobData(p.job.Type)
	assert.NoError(t, err)
	assert.Empty(t, v)

	p.mu.state = 1
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[1] = &metapb.ShardPool{Capacity: 1, Seq: 1, AllocatedOffset: 0}
	p.gcAllocating(ss, nil)
	v, err = ss.GetJobData(p.job.Type)
	assert.NoError(t, err)
	assert.Empty(t, v)

	aware := mockjob.NewMockShardsAware(ctrl)
	aware.EXPECT().GetShard(gomock.Eq(uint64(1))).Return(core.NewCachedShard(metapb.Shard{}, nil))
	aware.EXPECT().GetShard(gomock.Eq(uint64(2))).Return(core.NewCachedShard(metapb.Shard{}, nil, core.SetWrittenKeys(1)))
	p.mu.state = 1
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[1] = &metapb.ShardPool{Capacity: 2, Seq: 2, AllocatedOffset: 2, AllocatedShards: []*metapb.AllocatedShard{
		{
			ShardID:     1,
			AllocatedAt: 1,
		},
		{
			ShardID:     2,
			AllocatedAt: 2,
		},
	}}
	p.gcAllocating(ss, aware)
	v, err = ss.GetJobData(p.job.Type)
	assert.NoError(t, err)
	assert.NotEmpty(t, v)
	assert.Equal(t, 1, len(p.mu.pools.Pools[1].AllocatedShards))
	assert.Equal(t, uint64(1), p.mu.pools.Pools[1].AllocatedShards[0].ShardID)
}

func TestMaybeCreate(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ss := storage.NewTestStorage()
	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)
	p.job = metapb.Job{Type: metapb.JobType_CreateShardPool}

	c := 0
	ok := false
	client := mockclient.NewMockClient(ctrl)
	p.setProphetClient(client)
	client.EXPECT().AsyncAddShards(gomock.Any()).AnyTimes().DoAndReturn(func(resources ...interface{}) error {
		if !ok {
			return errors.New("error")
		}

		c += len(resources)
		return nil
	})

	// not start
	p.maybeCreate(ss)
	assert.Equal(t, 0, c)
	assert.Equal(t, 0, len(p.mu.createC))

	p.mu.state = 1
	p.mu.createC = make(chan struct{}, 10)
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[0] = &metapb.ShardPool{Capacity: uint64(batchCreateCount)}

	// pd return error
	ok = false
	p.maybeCreate(ss)
	assert.Equal(t, 0, c)
	assert.Equal(t, uint64(0), p.mu.pools.Pools[0].Seq)
	assert.Equal(t, 0, len(p.mu.createC))

	ok = true
	p.maybeCreate(ss)
	assert.Equal(t, c, batchCreateCount)
	assert.Equal(t, 1, len(p.mu.createC))
	assert.Equal(t, uint64(batchCreateCount), p.mu.pools.Pools[0].Seq)
	v, err := ss.GetJobData(p.job.Type)
	assert.NoError(t, err)
	assert.Equal(t, protoc.MustMarshal(&p.mu.pools), v)
}

func TestDoAllocLocked(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	aware := mockjob.NewMockShardsAware(ctrl)
	ss := storage.NewTestStorage()
	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)
	p.job = metapb.Job{Type: metapb.JobType_CreateShardPool}
	p.mu.state = 1
	p.mu.createC = make(chan struct{}, 10)

	// allocate retry again
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[0] = &metapb.ShardPool{Capacity: 1, Seq: 0, AllocatedOffset: 0}
	v, err := p.doAllocLocked(&metapb.ShardsPoolAllocCmd{Purpose: []byte("p1")}, ss, aware)
	assert.NoError(t, err)
	assert.Empty(t, v)
	assert.Equal(t, 1, len(p.mu.createC))

	// already allocated
	p.mu.createC = make(chan struct{}, 10)
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[0] = &metapb.ShardPool{Capacity: 1, Seq: 1, AllocatedOffset: 1, AllocatedShards: []*metapb.AllocatedShard{
		{
			ShardID:     1,
			AllocatedAt: 1,
			Purpose:     []byte("p1"),
		},
	}}
	v, err = p.doAllocLocked(&metapb.ShardsPoolAllocCmd{Purpose: []byte("p1")}, ss, aware)
	assert.NoError(t, err)
	assert.NotEmpty(t, v)
	assert.Equal(t, 1, len(p.mu.createC))

	// pd has no corresponding data
	aware = mockjob.NewMockShardsAware(ctrl)
	aware.EXPECT().ForeachShards(gomock.Any(), gomock.Any()).DoAndReturn(func(group uint64, fn func(res metapb.Shard)) {})
	aware.EXPECT().ForeachWaitingCreateShards(gomock.Any()).DoAndReturn(func(fn func(res metapb.Shard)) {})
	p.mu.createC = make(chan struct{}, 10)
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[0] = &metapb.ShardPool{Capacity: 1, Seq: 1, AllocatedOffset: 0}
	v, err = p.doAllocLocked(&metapb.ShardsPoolAllocCmd{Purpose: []byte("p1")}, ss, aware)
	assert.NoError(t, err)
	assert.Empty(t, v)
	assert.Equal(t, 0, len(p.mu.createC))
	assert.Equal(t, uint64(0), p.mu.pools.Pools[0].AllocatedOffset)

	// allocate from created
	aware = mockjob.NewMockShardsAware(ctrl)
	aware.EXPECT().ForeachWaitingCreateShards(gomock.Any()).DoAndReturn(func(fn func(res metapb.Shard)) {
		fn(metapb.Shard{ID: 1, Unique: p.unique(0, 1)})
	})
	p.mu.createC = make(chan struct{}, 10)
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[0] = &metapb.ShardPool{Capacity: 1, Seq: 1, AllocatedOffset: 0}
	v, err = p.doAllocLocked(&metapb.ShardsPoolAllocCmd{Purpose: []byte("p1")}, ss, aware)
	assert.NoError(t, err)
	assert.NotEmpty(t, v)
	assert.Equal(t, 1, len(p.mu.createC))
	assert.Equal(t, uint64(1), p.mu.pools.Pools[0].AllocatedOffset)
	v, err = ss.GetJobData(p.job.Type)
	assert.NoError(t, err)
	assert.Equal(t, protoc.MustMarshal(&p.mu.pools), v)
}

func TestShardsPoolStartAndStop(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s := storage.NewTestStorage()

	c := NewSingleTestClusterStore(t)
	c.Start()
	defer c.Stop()

	cfg := c.GetStore(0).GetConfig()
	p := newDynamicShardsPool(cfg, nil)
	p.setProphetClient(c.GetProphet().GetClient())

	p.Start(metapb.Job{Type: metapb.JobType_CreateShardPool, Content: protoc.MustMarshal(&metapb.ShardPoolJob{
		Pools: []metapb.ShardPoolJobMeta{{Group: 0, Capacity: 2}},
	})}, s, nil)
	p.mu.RLock()
	assert.Equal(t, 1, p.mu.state)
	assert.NotNil(t, p.mu.createC)
	assert.Equal(t, uint64(2), p.mu.pools.Pools[0].Capacity)
	p.mu.RUnlock()

	p.Start(metapb.Job{Type: metapb.JobType_CreateShardPool, Content: protoc.MustMarshal(&metapb.ShardPoolJob{
		Pools: []metapb.ShardPoolJobMeta{{Group: 0, Capacity: 3}},
	})}, s, nil)
	p.mu.RLock()
	assert.Equal(t, 1, p.mu.state)
	assert.NotNil(t, p.mu.createC)
	assert.Equal(t, uint64(2), p.mu.pools.Pools[0].Capacity)
	p.mu.RUnlock()

	p.mu.Lock()
	p.mu.state = 0
	p.mu.Unlock()
	job := metapb.Job{Type: metapb.JobType_CreateShardPool}
	pools := metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	pools.Pools[0] = &metapb.ShardPool{Capacity: 4}
	s.PutJobData(job.Type, protoc.MustMarshal(&pools))
	p.Start(job, s, nil)

	p.mu.RLock()
	assert.Equal(t, 1, p.mu.state)
	assert.NotNil(t, p.mu.createC)
	assert.Equal(t, uint64(4), p.mu.pools.Pools[0].Capacity)
	p.mu.RUnlock()

	p.Stop(job, s, nil)
	p.Stop(job, s, nil)
}

func TestExecute(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	aware := mockjob.NewMockShardsAware(ctrl)

	ss := storage.NewTestStorage()
	s, cancel := newTestStore(t)
	defer cancel()

	cfg := s.GetConfig()
	p := newDynamicShardsPool(cfg, nil)

	_, err := p.Execute(nil, ss, aware)
	assert.Error(t, err)

	_, err = p.Execute(make([]byte, 10), ss, aware)
	assert.Error(t, err)

	p.job = metapb.Job{Type: metapb.JobType_CreateShardPool}
	p.mu.Lock()
	p.mu.state = 1
	p.mu.createC = make(chan struct{}, 10)
	p.mu.pools = metapb.ShardsPool{Pools: make(map[uint64]*metapb.ShardPool)}
	p.mu.pools.Pools[0] = &metapb.ShardPool{Capacity: 1, Seq: 0, AllocatedOffset: 0}
	p.mu.Unlock()
	_, err = p.Execute(protoc.MustMarshal(&metapb.ShardsPoolCmd{Alloc: &metapb.ShardsPoolAllocCmd{Purpose: []byte("p1")}}), ss, aware)
	assert.Error(t, err)

	_, err = p.Execute(protoc.MustMarshal(&metapb.ShardsPoolCmd{Type: metapb.ShardsPoolCmdType_AllocShard, Alloc: &metapb.ShardsPoolAllocCmd{Purpose: []byte("p1")}}), ss, aware)
	assert.NoError(t, err)
	p.mu.RLock()
	assert.Equal(t, 1, len(p.mu.createC))
	p.mu.RUnlock()
}
