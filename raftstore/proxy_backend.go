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
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fagongzi/goetty"
	"github.com/fagongzi/goetty/codec"
	"github.com/fagongzi/goetty/codec/length"
	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/util/stop"
	"github.com/matrixorigin/matrixcube/util/task"
)

var (
	closeFlag = &struct{}{}

	errConnect            = errors.New("not connected")
	defaultConnectTimeout = time.Second * 10
)

type defaultBackendFactory struct {
	logger  *zap.Logger
	s       *store
	local   backend
	encoder codec.Encoder
	decoder codec.Decoder
}

func newBackendFactory(logger *zap.Logger, s *store) backendFactory {
	v := &rpcCodec{clientSide: true}
	encoder, decoder := length.NewWithSize(v, v, 0, 0, 0, int(s.cfg.Raft.MaxEntryBytes)*2)
	return &defaultBackendFactory{
		logger:  logger,
		s:       s,
		encoder: encoder,
		decoder: decoder,
		local:   newLocalBackend(s.OnRequest),
	}
}

func (f *defaultBackendFactory) create(addr string, success SuccessCallback, failure FailureCallback) (backend, error) {
	if addr == f.s.Meta().ClientAddress {
		return f.local, nil
	}

	return newRemoteBackend(f.logger, success, failure, addr, goetty.NewIOSession(goetty.WithCodec(f.encoder, f.decoder))),
		nil
}

type mockBackend struct {
	handler    func(rpcpb.Request) (rpcpb.ResponseBatch, error)
	onResponse func(rpcpb.ResponseBatch)
}

func newMockBackend(handler func(rpcpb.Request) (rpcpb.ResponseBatch, error), onResponse func(rpcpb.ResponseBatch)) backend {
	return &mockBackend{
		handler:    handler,
		onResponse: onResponse,
	}
}

func (mb *mockBackend) dispatch(req rpcpb.Request) error {
	req.PID = 0
	resp, err := mb.handler(req)
	if err != nil {
		return err
	}
	mb.onResponse(resp)
	return nil
}

func (mb *mockBackend) close() {
	mb.close()
}

type localBackend struct {
	handler func(rpcpb.Request) error
}

func newLocalBackend(handler func(rpcpb.Request) error) backend {
	return &localBackend{handler: handler}
}

func (lb *localBackend) dispatch(req rpcpb.Request) error {
	req.PID = 0
	return lb.handler(req)
}

func (lb *localBackend) close() {

}

type remoteBackend struct {
	sync.Mutex

	addr            string
	logger          *zap.Logger
	successCallback SuccessCallback
	failureCallback FailureCallback
	conn            goetty.IOSession
	reqs            *task.Queue
	stopper         *stop.Stopper
}

func newRemoteBackend(logger *zap.Logger,
	successCallback SuccessCallback,
	failureCallback FailureCallback,
	addr string,
	conn goetty.IOSession) *remoteBackend {
	bc := &remoteBackend{
		logger:          log.Adjust(logger).With(zap.String("remote", addr)),
		successCallback: successCallback,
		failureCallback: failureCallback,
		addr:            addr,
		conn:            conn,
		reqs:            task.New(32),
	}
	bc.stopper = stop.NewStopper(fmt.Sprintf("rpcpb-backend-%s", addr))
	bc.stopper.RunTask(context.Background(), bc.writeLoop)
	return bc
}

func (bc *remoteBackend) dispatch(req rpcpb.Request) error {
	if !bc.checkConnect() {
		return multierr.Append(errConnect, &ErrTryAgain{
			Wait: time.Second,
		})
	}

	return bc.reqs.Put(req)
}

func (bc *remoteBackend) close() {
	bc.reqs.Put(closeFlag)
	bc.stopper.Stop()
}

func (bc *remoteBackend) checkConnect() bool {
	if nil == bc {
		return false
	}

	if bc.conn.Connected() {
		return true
	}

	bc.Lock()
	defer bc.Unlock()

	if bc.conn.Connected() {
		return true
	}

	ok, err := bc.conn.Connect(bc.addr, defaultConnectTimeout)
	if err != nil {
		bc.logger.Error("fail to connect to backend",
			zap.Error(err))
		return false
	}

	bc.stopper.RunTask(context.Background(), bc.readLoop)
	return ok
}

func (bc *remoteBackend) writeLoop(ctx context.Context) {
	go func() {
		batch := int64(16)
		bc.logger.Info("backend write loop started")

		items := make([]interface{}, batch)
		for {
			n, err := bc.reqs.Get(batch, items)
			if err != nil {
				bc.logger.Fatal("BUG: fail to read from queue",
					zap.Error(err))
				return
			}

			for i := int64(0); i < n; i++ {
				if items[i] == closeFlag {
					bc.conn.Close()
					bc.logger.Info("backend write loop stopped")
					return
				}

				if ce := bc.logger.Check(zap.DebugLevel, "send request"); ce != nil {
					ce.Write(log.HexField("id", items[i].(rpcpb.Request).ID))
				}
				bc.conn.Write(items[i])
			}

			err = bc.conn.Flush()
			if err != nil {
				for i := int64(0); i < n; i++ {
					req := items[i].(rpcpb.Request)
					bc.failureCallback(req.ID, err)
				}
			}
		}
	}()
}

func (bc *remoteBackend) readLoop(ctx context.Context) {
	go func() {
		bc.logger.Info("backend read loop started")

		for {
			data, err := bc.conn.Read()
			if err != nil {
				bc.logger.Info("backend read loop stopped")
				bc.conn.Close()
				return
			}

			if rsp, ok := data.(rpcpb.Response); ok {
				if ce := bc.logger.Check(zap.DebugLevel, "backend received response"); ce != nil {
					ce.Write(log.HexField("id", rsp.ID),
						log.RaftResponseField("response", &rsp))
				}
				bc.successCallback(rsp)
			}
		}
	}()
}
