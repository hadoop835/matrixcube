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
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/matrixorigin/matrixcube/components/log"
	pconfig "github.com/matrixorigin/matrixcube/components/prophet/config"
	"github.com/matrixorigin/matrixcube/components/prophet/metadata"
	"github.com/matrixorigin/matrixcube/config"
	"github.com/matrixorigin/matrixcube/vfs"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestSingleStart(t *testing.T) {
	p := newTestSingleProphet(t, nil)
	defer p.Stop()

	assert.NotNil(t, p.GetMember())
	assert.True(t, p.GetMember().IsLeader())
	assert.True(t, p.GetClusterID() > 0)
}

func TestClusterStart(t *testing.T) {
	cluster := newTestClusterProphet(t, 4, nil)
	defer func() {
		for _, p := range cluster {
			p.Stop()
		}
	}()

	leaderCount := 0
	followerCount := 0
	for _, p := range cluster {
		assert.NotNil(t, p.GetMember())
		assert.True(t, p.GetClusterID() > 0)
		if p.GetMember().IsLeader() {
			leaderCount++
		} else {
			followerCount++
		}
	}
	assert.Equal(t, 1, leaderCount)
	assert.Equal(t, 3, followerCount)
}

func newTestSingleProphet(t *testing.T, adjustFunc func(*pconfig.Config)) Prophet {
	c := pconfig.NewConfig()
	c.ProphetNode = true
	c.TestContext = pconfig.NewTestContext()
	if adjustFunc != nil {
		adjustFunc(c)
	}
	return newTestProphet(t, c, vfs.GetTestFS())
}

func findProphetLeader(t *testing.T, cluster []Prophet, clusterSize int) Prophet {
	assert.Equal(t, len(cluster), clusterSize)

	var leader Prophet
	for i := 0; i < clusterSize; i++ {
		p := cluster[i]
		if p.GetLeader() != nil && p.GetLeader().ID == p.GetMember().ID() {
			leader = p
			break
		}
	}
	return leader
}

func newTestClusterProphet(t *testing.T, n int, adjustFunc func(*pconfig.Config)) []Prophet {
	if n < 3 {
		assert.FailNow(t, "cluster count must >= 3")
	}
	fs := vfs.GetTestFS()
	var cluster []Prophet
	for i := 0; i < n; i++ {
		if len(cluster) >= 2 {
			time.Sleep(time.Second * 5)
		}

		c := pconfig.NewConfig()
		c.Name = fmt.Sprintf("n-%d", i)
		c.DataDir = fmt.Sprintf("/tmp/prophet/%s", c.Name)
		c.RPCAddr = fmt.Sprintf("127.0.0.1:1000%d", i)
		c.TestContext = pconfig.NewTestContext()
		if i < 3 {
			c.ProphetNode = true
			c.EmbedEtcd.ClientUrls = fmt.Sprintf("http://127.0.0.1:2000%d", i)
			c.EmbedEtcd.PeerUrls = fmt.Sprintf("http://127.0.0.1:3000%d", i)
			c.EmbedEtcd.TickInterval.Duration = time.Millisecond * 50
			c.EmbedEtcd.ElectionInterval.Duration = time.Millisecond * 300

			if i != 0 {
				c.EmbedEtcd.Join = fmt.Sprintf("http://127.0.0.1:3000%d", 0)
			}
		} else {
			c.ExternalEtcd = []string{"http://127.0.0.1:20001", "http://127.0.0.1:20002", "http://127.0.0.1:20003"}
		}

		if adjustFunc != nil {
			adjustFunc(c)
		}
		cluster = append(cluster, newTestProphet(t, c, fs))
	}

	return cluster
}

func newTestProphet(t *testing.T, c *pconfig.Config, fs vfs.FS) Prophet {
	completedC := make(chan struct{})
	var completeOnce sync.Once
	cb := func() {
		completeOnce.Do(func() {
			completedC <- struct{}{}
		})
	}

	assert.NoError(t, c.Adjust(nil, false))
	assert.NoError(t, os.RemoveAll(c.DataDir))
	c.Handler = metadata.NewTestRoleHandler(cb, cb)
	p := NewProphet(&config.Config{
		Prophet: *c,
		Logger:  log.GetDefaultZapLoggerWithLevel(zap.DebugLevel).With(zap.String("testcase", t.Name()), zap.String("node", c.Name)),
		FS:      fs,
	})
	p.Start()
	select {
	case <-time.After(time.Second * 10):
		assert.Fail(t, "timeout")
	case <-completedC:
		break
	}
	return p
}
