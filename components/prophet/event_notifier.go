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

package prophet

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/fagongzi/goetty"
	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/components/prophet/cluster"
	"github.com/matrixorigin/matrixcube/components/prophet/event"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/util/stop"
	"go.uber.org/zap"
)

type watcherSession struct {
	seq     uint64
	flag    uint32
	session goetty.IOSession
}

func (wt *watcherSession) notify(evt rpcpb.EventNotify) error {
	if event.MatchEvent(evt.Type, wt.flag) {
		resp := &rpcpb.ProphetResponse{}
		resp.Type = rpcpb.TypeEventNotify
		resp.Event = evt
		resp.Event.Seq = atomic.AddUint64(&wt.seq, 1)
		return wt.session.WriteAndFlush(resp)
	}
	return nil
}

type eventNotifier struct {
	sync.Mutex

	logger   *zap.Logger
	watchers map[uint64]*watcherSession
	cluster  *cluster.RaftCluster
	stopper  *stop.Stopper
}

func newWatcherNotifier(cluster *cluster.RaftCluster, logger *zap.Logger) *eventNotifier {
	wn := &eventNotifier{
		logger:   log.Adjust(logger).Named("watch-notify"),
		cluster:  cluster,
		watchers: make(map[uint64]*watcherSession),
	}
	wn.stopper = stop.NewStopper("event-notifier", stop.WithLogger(wn.logger))
	return wn
}

func (wn *eventNotifier) handleCreateWatcher(req *rpcpb.ProphetRequest, resp *rpcpb.ProphetResponse, session goetty.IOSession) error {
	if wn != nil {
		wn.logger.Info("watcher added",
			zap.String("address", session.RemoteAddr()))

		wn.cluster.RLock()
		defer wn.cluster.RUnlock()
		if event.MatchEvent(event.InitEvent, req.CreateWatcher.Flag) {
			snap := event.Snapshot{
				Leaders: make(map[uint64]uint64),
			}
			for _, c := range wn.cluster.GetStores() {
				snap.Stores = append(snap.Stores, c.Meta)
			}
			for _, res := range wn.cluster.GetShards() {
				snap.Shards = append(snap.Shards, res.Meta)
				leader := res.GetLeader()
				if leader != nil {
					snap.Leaders[res.Meta.GetID()] = leader.ID
				}
			}

			rsp, err := event.NewInitEvent(snap)
			if err != nil {
				return err
			}

			resp.Event.Type = event.InitEvent
			resp.Event.InitEvent = rsp
		}

		return wn.addWatcher(req.CreateWatcher.Flag, session)
	}

	return nil
}

func (wn *eventNotifier) addWatcher(flag uint32, session goetty.IOSession) error {
	wn.Lock()
	defer wn.Unlock()

	if wn.watchers == nil {
		return fmt.Errorf("watcher notifier stopped")
	}

	wn.watchers[session.ID()] = &watcherSession{
		flag:    flag,
		session: session,
	}
	return nil
}

func (wn *eventNotifier) doClearWatcherLocked(w *watcherSession) {
	delete(wn.watchers, w.session.ID())
	w.session.Close()
	wn.logger.Info("watcher removed",
		zap.String("address", w.session.RemoteAddr()))
}

func (wn *eventNotifier) doNotify(evt rpcpb.EventNotify) {
	wn.Lock()
	defer wn.Unlock()

	for _, wt := range wn.watchers {
		err := wt.notify(evt)
		if err != nil {
			wn.doClearWatcherLocked(wt)
		}
	}
}

func (wn *eventNotifier) start() {
	wn.stopper.RunTask(context.Background(), func(ctx context.Context) {
		eventC := wn.cluster.ChangedEventNotifier()
		if eventC == nil {
			wn.logger.Info("watcher notifier exit with nil event channel")
			return
		}

		for {
			select {
			case <-ctx.Done():
				wn.logger.Info("watcher notifier exit")
				return
			case evt, ok := <-eventC:
				if !ok {
					wn.logger.Info("watcher notifier exit with channel closed")
					return
				}
				wn.doNotify(evt)
			}
		}
	})
}

func (wn *eventNotifier) stop() {
	wn.Lock()
	for _, wt := range wn.watchers {
		wn.doClearWatcherLocked(wt)
	}
	wn.watchers = nil
	wn.logger.Info("watcher notifier stopped")
	wn.Unlock()
	wn.stopper.Stop()
}
