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

package util

import (
	"bytes"
	"sync"

	"github.com/google/btree"
	"github.com/matrixorigin/matrixcube/pb/metapb"
)

const (
	defaultBTreeDegree = 64
)

var (
	emptyShard metapb.Shard
	itemPool   sync.Pool
)

func acquireItem() *ShardItem {
	v := itemPool.Get()
	if v == nil {
		return &ShardItem{}
	}
	return v.(*ShardItem)
}

func releaseItem(item *ShardItem) {
	itemPool.Put(item)
}

// ShardItem is the Shard btree item
type ShardItem struct {
	Shard metapb.Shard
}

// ShardTree is the btree for Shard
type ShardTree struct {
	sync.RWMutex
	tree *btree.BTree
}

// NewShardTree returns a default Shard btree
func NewShardTree() *ShardTree {
	return &ShardTree{
		tree: btree.New(defaultBTreeDegree),
	}
}

// Less returns true if the Shard start key is greater than the other.
// So we will sort the Shard with start key reversely.
func (r *ShardItem) Less(other btree.Item) bool {
	left := r.Shard.Start
	right := other.(*ShardItem).Shard.Start
	return bytes.Compare(left, right) > 0
}

// Contains returns the item contains the key
func (r *ShardItem) Contains(key []byte) bool {
	start, end := r.Shard.Start, r.Shard.End
	// len(end) == 0: max field is positive infinity
	return bytes.Compare(key, start) >= 0 && (len(end) == 0 || bytes.Compare(key, end) < 0)
}

func (t *ShardTree) length() int {
	return t.tree.Len()
}

// Update updates the tree with the Shard.
// It finds and deletes all the overlapped Shards first, and then
// insert the Shard.
func (t *ShardTree) Update(shards ...metapb.Shard) {
	t.Lock()
	for _, shard := range shards {
		if shard.State == metapb.ShardState_Destroyed ||
			shard.State == metapb.ShardState_Destroying {
			continue
		}

		item := &ShardItem{Shard: shard}

		result := t.find(shard)
		if result == nil {
			result = item
		}

		var overlaps []*ShardItem

		// between [Shard, first], so is iterator all.min >= Shard.min' Shard
		// until all.min > Shard.max
		t.tree.DescendLessOrEqual(result, func(i btree.Item) bool {
			over := i.(*ShardItem)
			// Shard.max <= i.start, so Shard and i has no overlaps,
			// otherwise Shard and i has overlaps
			if len(shard.End) > 0 && bytes.Compare(shard.End, over.Shard.Start) <= 0 {
				return false
			}
			overlaps = append(overlaps, over)
			return true
		})

		for _, item := range overlaps {
			t.tree.Delete(item)
		}

		t.tree.ReplaceOrInsert(item)
	}
	t.Unlock()
}

// Remove removes a Shard if the Shard is in the tree.
// It will do nothing if it cannot find the Shard or the found Shard
// is not the same with the Shard.
func (t *ShardTree) Remove(shard metapb.Shard) bool {
	t.Lock()

	result := t.find(shard)
	if result == nil || result.Shard.ID != shard.ID {
		t.Unlock()
		return false
	}

	t.tree.Delete(result)
	t.Unlock()
	return true
}

// Ascend asc iterator the tree until fn returns false
func (t *ShardTree) Ascend(fn func(Shard *metapb.Shard) bool) {
	t.RLock()
	t.tree.Descend(func(item btree.Item) bool {
		return fn(&item.(*ShardItem).Shard)
	})
	t.RUnlock()
}

// NextShard return the next bigger key range Shard
func (t *ShardTree) NextShard(start []byte) *metapb.Shard {
	var value *ShardItem

	p := &ShardItem{
		Shard: metapb.Shard{Start: start},
	}

	t.RLock()
	t.tree.DescendLessOrEqual(p, func(item btree.Item) bool {
		if bytes.Compare(item.(*ShardItem).Shard.Start, start) > 0 {
			value = item.(*ShardItem)
			return false
		}

		return true
	})
	t.RUnlock()

	if nil == value {
		return nil
	}

	return &value.Shard
}

// AscendRange asc iterator the tree in the range [start, end) until fn returns false
func (t *ShardTree) AscendRange(start, end []byte, fn func(shard *metapb.Shard) bool) {
	t.RLock()
	defer t.RUnlock()

	startShard := t.find(metapb.Shard{Start: start})
	if startShard == nil {
		return
	}
	t.tree.DescendLessOrEqual(startShard, func(item btree.Item) bool {
		if len(end) > 0 && bytes.Compare(item.(*ShardItem).Shard.Start, end) >= 0 {
			return false
		}

		return fn(&item.(*ShardItem).Shard)
	})
}

// Search returns a Shard that contains the key.
func (t *ShardTree) Search(key []byte) metapb.Shard {
	shard := metapb.Shard{Start: key}

	t.RLock()
	result := t.find(shard)
	t.RUnlock()

	if result == nil {
		return emptyShard
	}

	return result.Shard
}

func (t *ShardTree) find(shard metapb.Shard) *ShardItem {
	item := acquireItem()
	item.Shard = shard

	var result *ShardItem
	t.tree.AscendGreaterOrEqual(item, func(i btree.Item) bool {
		result = i.(*ShardItem)
		return false
	})

	if result == nil || !result.Contains(shard.Start) {
		releaseItem(item)
		return nil
	}

	releaseItem(item)
	return result
}
