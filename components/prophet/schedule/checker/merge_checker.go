// Copyright 2020 PingCAP, Inc.
// Modifications copyright (C) 2021 MatrixOrigin.
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

package checker

import (
	"bytes"
	"context"
	"time"

	"github.com/matrixorigin/matrixcube/components/prophet/config"
	"github.com/matrixorigin/matrixcube/components/prophet/core"
	"github.com/matrixorigin/matrixcube/components/prophet/schedule/operator"
	"github.com/matrixorigin/matrixcube/components/prophet/schedule/opt"
	"github.com/matrixorigin/matrixcube/components/prophet/schedule/placement"
	"github.com/matrixorigin/matrixcube/components/prophet/util"
	"go.uber.org/zap"
)

const maxTargetShardSize = 500

// MergeChecker ensures resource to merge with adjacent resource when size is small
type MergeChecker struct {
	cluster    opt.Cluster
	opts       *config.PersistOptions
	splitCache *util.TTLUint64
	startTime  time.Time // it's used to judge whether server recently start.
}

// NewMergeChecker creates a merge checker.
func NewMergeChecker(ctx context.Context, cluster opt.Cluster) *MergeChecker {
	opts := cluster.GetOpts()
	splitCache := util.NewIDTTL(ctx, time.Minute, opts.GetSplitMergeInterval())
	return &MergeChecker{
		cluster:    cluster,
		opts:       opts,
		splitCache: splitCache,
		startTime:  time.Now(),
	}
}

// GetType return MergeChecker's type
func (m *MergeChecker) GetType() string {
	return "merge-checker"
}

// RecordShardSplit put the recently split resource into cache. MergeChecker
// will skip check it for a while.
func (m *MergeChecker) RecordShardSplit(resourceIDs []uint64) {
	for _, resID := range resourceIDs {
		m.splitCache.PutWithTTL(resID, nil, m.opts.GetSplitMergeInterval())
	}
}

// Check verifies a resource's replicas, creating an Operator if need.
func (m *MergeChecker) Check(res *core.CachedShard) []*operator.Operator {
	expireTime := m.startTime.Add(m.opts.GetSplitMergeInterval())
	if time.Now().Before(expireTime) {
		checkerCounter.WithLabelValues("merge_checker", "recently-start").Inc()
		return nil
	}

	if m.splitCache.Exists(res.Meta.GetID()) {
		checkerCounter.WithLabelValues("merge_checker", "recently-split").Inc()
		return nil
	}

	checkerCounter.WithLabelValues("merge_checker", "check").Inc()

	// when pd just started, it will load resource meta from etcd
	// but the size for these loaded resource info is 0
	// pd don't know the real size of one resource until the first heartbeat of the resource
	// thus here when size is 0, just skip.
	if res.GetApproximateSize() == 0 {
		checkerCounter.WithLabelValues("merge_checker", "skip").Inc()
		return nil
	}

	// resource is not small enough
	if res.GetApproximateSize() > int64(m.opts.GetMaxMergeShardSize()) ||
		res.GetApproximateKeys() > int64(m.opts.GetMaxMergeShardKeys()) {
		checkerCounter.WithLabelValues("merge_checker", "no-need").Inc()
		return nil
	}

	// skip resource has down peers or pending peers or learner peers
	if !opt.IsShardHealthy(m.cluster, res) {
		checkerCounter.WithLabelValues("merge_checker", "special-peer").Inc()
		return nil
	}

	if !opt.IsShardReplicated(m.cluster, res) {
		checkerCounter.WithLabelValues("merge_checker", "abnormal-replica").Inc()
		return nil
	}

	// skip hot resource
	if m.cluster.IsShardHot(res) {
		checkerCounter.WithLabelValues("merge_checker", "hot-resource").Inc()
		return nil
	}

	prev, next := m.cluster.GetAdjacentShards(res)

	var target *core.CachedShard
	if m.checkTarget(res, next) {
		target = next
	}
	if !m.opts.IsOneWayMergeEnabled() && m.checkTarget(res, prev) { // allow a resource can be merged by two ways.
		if target == nil || prev.GetApproximateSize() < next.GetApproximateSize() { // pick smaller
			target = prev
		}
	}

	if target == nil {
		checkerCounter.WithLabelValues("merge_checker", "no-target").Inc()
		return nil
	}

	if target.GetApproximateSize() > maxTargetShardSize {
		checkerCounter.WithLabelValues("merge_checker", "target-too-large").Inc()
		return nil
	}

	m.cluster.GetLogger().Debug("try to merge resource",
		zap.Stringer("from", core.ShardToHexMeta(res.Meta)),
		zap.Stringer("to", core.ShardToHexMeta(target.Meta)))

	ops, err := operator.CreateMergeShardOperator("merge-resource", m.cluster, res, target, operator.OpMerge)
	if err != nil {
		m.cluster.GetLogger().Warn("fail to create merge resource operator",
			zap.Error(err))
		return nil
	}
	checkerCounter.WithLabelValues("merge_checker", "new-operator").Inc()
	if res.GetApproximateSize() > target.GetApproximateSize() ||
		res.GetApproximateKeys() > target.GetApproximateKeys() {
		checkerCounter.WithLabelValues("merge_checker", "larger-source").Inc()
	}
	return ops
}

func (m *MergeChecker) checkTarget(region, adjacent *core.CachedShard) bool {
	return adjacent != nil && !m.splitCache.Exists(adjacent.Meta.GetID()) && !m.cluster.IsShardHot(adjacent) &&
		AllowMerge(m.cluster, region, adjacent) && opt.IsShardHealthy(m.cluster, adjacent) &&
		opt.IsShardReplicated(m.cluster, adjacent)
}

// AllowMerge returns true if two resources can be merged according to the key type.
func AllowMerge(cluster opt.Cluster, res *core.CachedShard, adjacent *core.CachedShard) bool {
	var start, end []byte
	if bytes.Equal(res.GetEndKey(), adjacent.GetStartKey()) && len(res.GetEndKey()) != 0 {
		start, end = res.GetStartKey(), adjacent.GetEndKey()
	} else if bytes.Equal(adjacent.GetEndKey(), res.GetStartKey()) && len(adjacent.GetEndKey()) != 0 {
		start, end = adjacent.GetStartKey(), res.GetEndKey()
	} else {
		return false
	}
	if cluster.GetOpts().IsPlacementRulesEnabled() {
		type withRuleManager interface {
			GetRuleManager() *placement.RuleManager
		}
		cl, ok := cluster.(withRuleManager)
		if !ok || len(cl.GetRuleManager().GetSplitKeys(start, end)) > 0 {
			return false
		}
	}

	return true
}
