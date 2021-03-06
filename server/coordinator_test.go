// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"math/rand"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/eraftpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/pd/pkg/testutil"
	"github.com/pingcap/pd/server/core"
	"github.com/pingcap/pd/server/namespace"
	"github.com/pingcap/pd/server/schedule"
	"github.com/pingcap/pd/server/schedulers"
	"github.com/pkg/errors"
)

func newTestOperator(regionID uint64, regionEpoch *metapb.RegionEpoch, kind schedule.OperatorKind) *schedule.Operator {
	return schedule.NewOperator("test", regionID, regionEpoch, kind)
}

func newTestScheduleConfig() (*ScheduleConfig, *scheduleOption) {
	cfg := NewConfig()
	cfg.Adjust(nil)
	opt := newScheduleOption(cfg)
	opt.SetClusterVersion(MinSupportedVersion(Version2_0))
	return &cfg.Schedule, opt
}

type testClusterInfo struct {
	*clusterInfo
}

func newTestClusterInfo(opt *scheduleOption) *testClusterInfo {
	return &testClusterInfo{clusterInfo: newClusterInfo(
		core.NewMockIDAllocator(),
		opt,
		core.NewKV(core.NewMemoryKV()),
	)}
}

func newTestRegionMeta(regionID uint64) *metapb.Region {
	return &metapb.Region{
		Id:          regionID,
		StartKey:    []byte(fmt.Sprintf("%20d", regionID)),
		EndKey:      []byte(fmt.Sprintf("%20d", regionID+1)),
		RegionEpoch: &metapb.RegionEpoch{Version: 1, ConfVer: 1},
	}
}

func (c *testClusterInfo) addRegionStore(storeID uint64, regionCount int) {
	store := core.NewStoreInfo(&metapb.Store{Id: storeID})
	store.Stats = &pdpb.StoreStats{}
	store.LastHeartbeatTS = time.Now()
	store.RegionCount = regionCount
	store.RegionSize = int64(regionCount) * 10
	store.Stats.Capacity = 1000 * (1 << 20)
	store.Stats.Available = store.Stats.Capacity - uint64(store.RegionSize)
	c.putStore(store)
}

func (c *testClusterInfo) addLeaderRegion(regionID uint64, leaderID uint64, followerIds ...uint64) {
	region := newTestRegionMeta(regionID)
	leader, _ := c.AllocPeer(leaderID)
	region.Peers = []*metapb.Peer{leader}
	for _, id := range followerIds {
		peer, _ := c.AllocPeer(id)
		region.Peers = append(region.Peers, peer)
	}
	regionInfo := core.NewRegionInfo(region, leader, core.SetApproximateSize(10), core.SetApproximateKeys(10))
	c.putRegion(regionInfo)
}

func (c *testClusterInfo) updateLeaderCount(storeID uint64, leaderCount int) {
	store := c.GetStore(storeID)
	store.LeaderCount = leaderCount
	store.LeaderSize = int64(leaderCount) * 10
	c.putStore(store)
}

func (c *testClusterInfo) addLeaderStore(storeID uint64, leaderCount int) {
	store := core.NewStoreInfo(&metapb.Store{Id: storeID})
	store.Stats = &pdpb.StoreStats{}
	store.LastHeartbeatTS = time.Now()
	store.LeaderCount = leaderCount
	store.LeaderSize = int64(leaderCount) * 10
	c.putStore(store)
}

func (c *testClusterInfo) setStoreDown(storeID uint64) {
	store := c.GetStore(storeID)
	store.State = metapb.StoreState_Up
	store.LastHeartbeatTS = time.Time{}
	c.putStore(store)
}

func (c *testClusterInfo) setStoreOffline(storeID uint64) {
	store := c.GetStore(storeID)
	store.State = metapb.StoreState_Offline
	c.putStore(store)
}

func (c *testClusterInfo) LoadRegion(regionID uint64, followerIds ...uint64) {
	//  regions load from etcd will have no leader
	region := newTestRegionMeta(regionID)
	region.Peers = []*metapb.Peer{}
	for _, id := range followerIds {
		peer, _ := c.AllocPeer(id)
		region.Peers = append(region.Peers, peer)
	}
	c.putRegion(core.NewRegionInfo(region, nil))
}

var _ = Suite(&testCoordinatorSuite{})

type testCoordinatorSuite struct{}

func (s *testCoordinatorSuite) TestBasic(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.clusterInfo.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	oc := co.opController

	tc.addLeaderRegion(1, 1)

	op1 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), schedule.OpLeader)
	oc.AddOperator(op1)
	c.Assert(oc.OperatorCount(op1.Kind()), Equals, uint64(1))
	c.Assert(oc.GetOperator(1).RegionID(), Equals, op1.RegionID())

	// Region 1 already has an operator, cannot add another one.
	op2 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), schedule.OpRegion)
	oc.AddOperator(op2)
	c.Assert(oc.OperatorCount(op2.Kind()), Equals, uint64(0))

	// Remove the operator manually, then we can add a new operator.
	oc.RemoveOperator(op1)
	oc.AddOperator(op2)
	c.Assert(oc.OperatorCount(op2.Kind()), Equals, uint64(1))
	c.Assert(oc.GetOperator(1).RegionID(), Equals, op2.RegionID())
}

type mockHeartbeatStream struct {
	ch chan *pdpb.RegionHeartbeatResponse
}

func (s *mockHeartbeatStream) Send(m *pdpb.RegionHeartbeatResponse) error {
	select {
	case <-time.After(time.Second):
		return errors.New("timeout")
	case s.ch <- m:
	}
	return nil
}

func (s *mockHeartbeatStream) Recv() *pdpb.RegionHeartbeatResponse {
	select {
	case <-time.After(time.Millisecond * 10):
		return nil
	case res := <-s.ch:
		return res
	}
}

func newMockHeartbeatStream() *mockHeartbeatStream {
	return &mockHeartbeatStream{
		ch: make(chan *pdpb.RegionHeartbeatResponse),
	}
}

func (s *testCoordinatorSuite) TestDispatch(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	defer co.wg.Wait()
	defer co.stop()

	// Transfer peer from store 4 to store 1.
	tc.addRegionStore(4, 40)
	tc.addRegionStore(3, 30)
	tc.addRegionStore(2, 20)
	tc.addRegionStore(1, 10)
	tc.addLeaderRegion(1, 2, 3, 4)

	// Transfer leader from store 4 to store 2.
	tc.updateLeaderCount(4, 50)
	tc.updateLeaderCount(3, 30)
	tc.updateLeaderCount(2, 20)
	tc.updateLeaderCount(1, 10)
	tc.addLeaderRegion(2, 4, 3, 2)

	// Wait for schedule and turn off balance.
	waitOperator(c, co, 1)
	testutil.CheckTransferPeer(c, co.opController.GetOperator(1), schedule.OpBalance, 4, 1)
	c.Assert(co.removeScheduler("balance-region-scheduler"), IsNil)
	waitOperator(c, co, 2)
	testutil.CheckTransferLeader(c, co.opController.GetOperator(2), schedule.OpBalance, 4, 2)
	c.Assert(co.removeScheduler("balance-leader-scheduler"), IsNil)

	stream := newMockHeartbeatStream()

	// Transfer peer.
	region := tc.GetRegion(1).Clone()
	dispatchHeartbeat(c, co, region, stream)
	region = waitAddLearner(c, stream, region, 1)
	dispatchHeartbeat(c, co, region, stream)
	region = waitPromoteLearner(c, stream, region, 1)
	dispatchHeartbeat(c, co, region, stream)
	region = waitRemovePeer(c, stream, region, 4)
	dispatchHeartbeat(c, co, region, stream)
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)

	// Transfer leader.
	region = tc.GetRegion(2).Clone()
	dispatchHeartbeat(c, co, region, stream)
	waitTransferLeader(c, stream, region, 2)
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)
}

func dispatchHeartbeat(c *C, co *coordinator, region *core.RegionInfo, stream *mockHeartbeatStream) {
	co.hbStreams.bindStream(region.GetLeader().GetStoreId(), stream)
	co.cluster.putRegion(region.Clone())
	co.opController.Dispatch(region)
}

func (s *testCoordinatorSuite) TestCollectMetrics(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	// Make sure there are no problem when concurrent write and read
	for i := 0; i <= 10; i++ {
		go func(i int) {
			for j := 0; j < 10000; j++ {
				tc.addRegionStore(uint64(i%5), rand.Intn(200))
			}
		}(i)
	}
	for i := 0; i < 1000; i++ {
		co.collectHotSpotMetrics()
		co.collectSchedulerMetrics()
		co.cluster.collectMetrics()
	}
}

func (s *testCoordinatorSuite) TestCheckRegion(c *C) {
	cfg, opt := newTestScheduleConfig()
	cfg.DisableLearner = false
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()

	tc.addRegionStore(4, 4)
	tc.addRegionStore(3, 3)
	tc.addRegionStore(2, 2)
	tc.addRegionStore(1, 1)
	tc.addLeaderRegion(1, 2, 3)
	c.Assert(co.checkRegion(tc.GetRegion(1)), IsTrue)
	waitOperator(c, co, 1)
	testutil.CheckAddPeer(c, co.opController.GetOperator(1), schedule.OpReplica, 1)
	c.Assert(co.checkRegion(tc.GetRegion(1)), IsFalse)

	r := tc.GetRegion(1)
	p := &metapb.Peer{Id: 1, StoreId: 1, IsLearner: true}
	r = r.Clone(
		core.WithAddPeer(p),
		core.WithPendingPeers(append(r.GetPendingPeers(), p)),
	)
	tc.putRegion(r)
	c.Assert(co.checkRegion(tc.GetRegion(1)), IsFalse)
	co.stop()
	co.wg.Wait()

	// new cluster with learner disabled
	cfg.DisableLearner = true
	tc = newTestClusterInfo(opt)
	co = newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	defer co.wg.Wait()
	defer co.stop()

	tc.addRegionStore(4, 4)
	tc.addRegionStore(3, 3)
	tc.addRegionStore(2, 2)
	tc.addRegionStore(1, 1)
	tc.putRegion(r)
	c.Assert(co.checkRegion(tc.GetRegion(1)), IsFalse)
	r = r.Clone(core.WithPendingPeers(nil))
	tc.putRegion(r)
	c.Assert(co.checkRegion(tc.GetRegion(1)), IsTrue)
	waitOperator(c, co, 1)
	op := co.opController.GetOperator(1)
	c.Assert(op.Len(), Equals, 1)
	c.Assert(op.Step(0).(schedule.PromoteLearner).ToStore, Equals, uint64(1))
	c.Assert(co.checkRegion(tc.GetRegion(1)), IsFalse)
}

func (s *testCoordinatorSuite) TestReplica(c *C) {
	// Turn off balance.
	cfg, opt := newTestScheduleConfig()
	cfg.LeaderScheduleLimit = 0
	cfg.RegionScheduleLimit = 0

	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	defer co.wg.Wait()
	defer co.stop()

	tc.addRegionStore(1, 1)
	tc.addRegionStore(2, 2)
	tc.addRegionStore(3, 3)
	tc.addRegionStore(4, 4)

	stream := newMockHeartbeatStream()

	// Add peer to store 1.
	tc.addLeaderRegion(1, 2, 3)
	region := tc.GetRegion(1)
	dispatchHeartbeat(c, co, region, stream)
	region = waitAddLearner(c, stream, region, 1)
	dispatchHeartbeat(c, co, region, stream)
	region = waitPromoteLearner(c, stream, region, 1)
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)

	// Peer in store 3 is down, remove peer in store 3 and add peer to store 4.
	tc.setStoreDown(3)
	downPeer := &pdpb.PeerStats{
		Peer:        region.GetStorePeer(3),
		DownSeconds: 24 * 60 * 60,
	}
	region = region.Clone(
		core.WithDownPeers(append(region.GetDownPeers(), downPeer)),
	)
	dispatchHeartbeat(c, co, region, stream)
	region = waitAddLearner(c, stream, region, 4)
	dispatchHeartbeat(c, co, region, stream)
	region = waitPromoteLearner(c, stream, region, 4)
	region = region.Clone(core.WithDownPeers(nil))
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)

	// Remove peer from store 4.
	tc.addLeaderRegion(2, 1, 2, 3, 4)
	region = tc.GetRegion(2)
	dispatchHeartbeat(c, co, region, stream)
	region = waitRemovePeer(c, stream, region, 4)
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)

	// Remove offline peer directly when it's pending.
	tc.addLeaderRegion(3, 1, 2, 3)
	tc.setStoreOffline(3)
	region = tc.GetRegion(3)
	region = region.Clone(core.WithPendingPeers([]*metapb.Peer{region.GetStorePeer(3)}))
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)
}

func (s *testCoordinatorSuite) TestPeerState(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	defer co.wg.Wait()
	defer co.stop()

	// Transfer peer from store 4 to store 1.
	tc.addRegionStore(1, 10)
	tc.addRegionStore(2, 20)
	tc.addRegionStore(3, 30)
	tc.addRegionStore(4, 40)
	tc.addLeaderRegion(1, 2, 3, 4)

	stream := newMockHeartbeatStream()

	// Wait for schedule.
	waitOperator(c, co, 1)
	testutil.CheckTransferPeer(c, co.opController.GetOperator(1), schedule.OpBalance, 4, 1)

	region := tc.GetRegion(1).Clone()

	// Add new peer.
	dispatchHeartbeat(c, co, region, stream)
	region = waitAddLearner(c, stream, region, 1)
	dispatchHeartbeat(c, co, region, stream)
	region = waitPromoteLearner(c, stream, region, 1)

	// If the new peer is pending, the operator will not finish.
	region = region.Clone(core.WithPendingPeers(append(region.GetPendingPeers(), region.GetStorePeer(1))))
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)
	c.Assert(co.opController.GetOperator(region.GetID()), NotNil)

	// The new peer is not pending now, the operator will finish.
	// And we will proceed to remove peer in store 4.
	region = region.Clone(core.WithPendingPeers(nil))
	dispatchHeartbeat(c, co, region, stream)
	waitRemovePeer(c, stream, region, 4)
	tc.addLeaderRegion(1, 1, 2, 3)
	region = tc.GetRegion(1).Clone()
	dispatchHeartbeat(c, co, region, stream)
	waitNoResponse(c, stream)
}

func (s *testCoordinatorSuite) TestShouldRun(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)

	tc.addLeaderStore(1, 5)
	tc.addLeaderStore(2, 2)
	tc.addLeaderStore(3, 0)
	tc.addLeaderStore(4, 0)
	tc.LoadRegion(1, 1, 2, 3)
	tc.LoadRegion(2, 1, 2, 3)
	tc.LoadRegion(3, 1, 2, 3)
	tc.LoadRegion(4, 1, 2, 3)
	tc.LoadRegion(5, 1, 2, 3)
	tc.LoadRegion(6, 2, 1, 4)
	tc.LoadRegion(7, 2, 1, 4)
	c.Assert(co.shouldRun(), IsFalse)
	c.Assert(tc.core.Regions.GetStoreRegionCount(4), Equals, 2)

	tbl := []struct {
		regionID  uint64
		shouldRun bool
	}{
		{1, false},
		{2, false},
		{3, false},
		{4, false},
		{5, false},
		// store4 needs collect two region
		{6, false},
		{7, true},
	}

	for _, t := range tbl {
		r := tc.GetRegion(t.regionID)
		nr := r.Clone(core.WithLeader(r.GetPeers()[0]))
		tc.handleRegionHeartbeat(nr)
		c.Assert(co.shouldRun(), Equals, t.shouldRun)
	}
	nr := &metapb.Region{Id: 6, Peers: []*metapb.Peer{}}
	newRegion := core.NewRegionInfo(nr, nil)
	tc.handleRegionHeartbeat(newRegion)
	c.Assert(co.cluster.prepareChecker.sum, Equals, 7)

}

func (s *testCoordinatorSuite) TestAddScheduler(c *C) {
	cfg, opt := newTestScheduleConfig()
	cfg.ReplicaScheduleLimit = 0

	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()
	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	defer co.wg.Wait()
	defer co.stop()

	c.Assert(co.schedulers, HasLen, 4)
	c.Assert(co.removeScheduler("balance-leader-scheduler"), IsNil)
	c.Assert(co.removeScheduler("balance-region-scheduler"), IsNil)
	c.Assert(co.removeScheduler("balance-hot-region-scheduler"), IsNil)
	c.Assert(co.removeScheduler("label-scheduler"), IsNil)
	c.Assert(co.schedulers, HasLen, 0)

	stream := newMockHeartbeatStream()

	// Add stores 1,2,3
	tc.addLeaderStore(1, 1)
	tc.addLeaderStore(2, 1)
	tc.addLeaderStore(3, 1)
	// Add regions 1 with leader in store 1 and followers in stores 2,3
	tc.addLeaderRegion(1, 1, 2, 3)
	// Add regions 2 with leader in store 2 and followers in stores 1,3
	tc.addLeaderRegion(2, 2, 1, 3)
	// Add regions 3 with leader in store 3 and followers in stores 1,2
	tc.addLeaderRegion(3, 3, 1, 2)

	oc := co.opController
	gls, err := schedule.CreateScheduler("grant-leader", oc, "0")
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls), NotNil)
	c.Assert(co.removeScheduler(gls.GetName()), NotNil)

	gls, err = schedule.CreateScheduler("grant-leader", oc, "1")
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls), IsNil)

	// Transfer all leaders to store 1.
	waitOperator(c, co, 2)
	region2 := tc.GetRegion(2)
	dispatchHeartbeat(c, co, region2, stream)
	region2 = waitTransferLeader(c, stream, region2, 1)
	dispatchHeartbeat(c, co, region2, stream)
	waitNoResponse(c, stream)

	waitOperator(c, co, 3)
	region3 := tc.GetRegion(3)
	dispatchHeartbeat(c, co, region3, stream)
	region3 = waitTransferLeader(c, stream, region3, 1)
	dispatchHeartbeat(c, co, region3, stream)
	waitNoResponse(c, stream)
}

func (s *testCoordinatorSuite) TestPersistScheduler(c *C) {
	cfg, opt := newTestScheduleConfig()
	cfg.ReplicaScheduleLimit = 0

	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()

	// Add stores 1,2
	tc.addLeaderStore(1, 1)
	tc.addLeaderStore(2, 1)

	c.Assert(co.schedulers, HasLen, 4)
	oc := co.opController
	gls1, err := schedule.CreateScheduler("grant-leader", oc, "1")
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls1, "1"), IsNil)
	gls2, err := schedule.CreateScheduler("grant-leader", oc, "2")
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(gls2, "2"), IsNil)
	c.Assert(co.schedulers, HasLen, 6)
	fmt.Println(opt)
	c.Assert(co.removeScheduler("balance-leader-scheduler"), IsNil)
	c.Assert(co.removeScheduler("balance-region-scheduler"), IsNil)
	c.Assert(co.removeScheduler("balance-hot-region-scheduler"), IsNil)
	c.Assert(co.removeScheduler("label-scheduler"), IsNil)
	c.Assert(co.schedulers, HasLen, 2)
	c.Assert(co.cluster.opt.persist(co.cluster.kv), IsNil)
	co.stop()
	co.wg.Wait()
	// make a new coordinator for testing
	// whether the schedulers added or removed in dynamic way are recorded in opt
	_, newOpt := newTestScheduleConfig()
	_, err = schedule.CreateScheduler("adjacent-region", oc)
	c.Assert(err, IsNil)
	// suppose we add a new default enable scheduler
	newOpt.AddSchedulerCfg("adjacent-region", []string{})
	c.Assert(newOpt.GetSchedulers(), HasLen, 5)
	newOpt.reload(co.cluster.kv)
	c.Assert(newOpt.GetSchedulers(), HasLen, 7)
	tc.clusterInfo.opt = newOpt

	co = newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	c.Assert(co.schedulers, HasLen, 3)
	co.stop()
	co.wg.Wait()
	// suppose restart PD again
	_, newOpt = newTestScheduleConfig()
	newOpt.reload(tc.kv)
	tc.clusterInfo.opt = newOpt
	co = newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	c.Assert(co.schedulers, HasLen, 3)
	bls, err := schedule.CreateScheduler("balance-leader", oc)
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(bls), IsNil)
	brs, err := schedule.CreateScheduler("balance-region", oc)
	c.Assert(err, IsNil)
	c.Assert(co.addScheduler(brs), IsNil)
	c.Assert(co.schedulers, HasLen, 5)
	// the scheduler option should contain 7 items
	// the `hot scheduler` and `label scheduler` are disabled
	c.Assert(co.cluster.opt.GetSchedulers(), HasLen, 7)
	c.Assert(co.removeScheduler("grant-leader-scheduler-1"), IsNil)
	// the scheduler that is not enable by default will be completely deleted
	c.Assert(co.cluster.opt.GetSchedulers(), HasLen, 6)
	c.Assert(co.schedulers, HasLen, 4)
	c.Assert(co.cluster.opt.persist(co.cluster.kv), IsNil)
	co.stop()
	co.wg.Wait()

	_, newOpt = newTestScheduleConfig()
	newOpt.reload(co.cluster.kv)
	tc.clusterInfo.opt = newOpt
	co = newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)

	co.run()
	defer co.wg.Wait()
	defer co.stop()
	c.Assert(co.schedulers, HasLen, 4)
	c.Assert(co.removeScheduler("grant-leader-scheduler-2"), IsNil)
	c.Assert(co.schedulers, HasLen, 3)
}

func (s *testCoordinatorSuite) TestRestart(c *C) {
	// Turn off balance, we test add replica only.
	cfg, opt := newTestScheduleConfig()
	cfg.LeaderScheduleLimit = 0
	cfg.RegionScheduleLimit = 0

	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	// Add 3 stores (1, 2, 3) and a region with 1 replica on store 1.
	tc.addRegionStore(1, 1)
	tc.addRegionStore(2, 2)
	tc.addRegionStore(3, 3)
	tc.addLeaderRegion(1, 1)
	region := tc.GetRegion(1)
	tc.prepareChecker.collect(region)

	// Add 1 replica on store 2.
	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	stream := newMockHeartbeatStream()
	dispatchHeartbeat(c, co, region, stream)
	region = waitAddLearner(c, stream, region, 2)
	dispatchHeartbeat(c, co, region, stream)
	region = waitPromoteLearner(c, stream, region, 2)
	co.stop()
	co.wg.Wait()

	// Recreate coodinator then add another replica on store 3.
	co = newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	co.run()
	dispatchHeartbeat(c, co, region, stream)
	region = waitAddLearner(c, stream, region, 3)
	dispatchHeartbeat(c, co, region, stream)
	waitPromoteLearner(c, stream, region, 3)
	co.stop()
	co.wg.Wait()
}

func waitOperator(c *C, co *coordinator, regionID uint64) {
	testutil.WaitUntil(c, func(c *C) bool {
		return co.opController.GetOperator(regionID) != nil
	})
}

var _ = Suite(&testOperatorControllerSuite{})

type testOperatorControllerSuite struct{}

func (s *testOperatorControllerSuite) TestOperatorCount(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := schedule.NewMockHeartbeatStreams(tc.clusterInfo.getClusterID())

	oc := schedule.NewOperatorController(tc.clusterInfo, hbStreams)
	c.Assert(oc.OperatorCount(schedule.OpLeader), Equals, uint64(0))
	c.Assert(oc.OperatorCount(schedule.OpRegion), Equals, uint64(0))

	tc.addLeaderRegion(1, 1)
	tc.addLeaderRegion(2, 2)
	op1 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), schedule.OpLeader)
	oc.AddOperator(op1)
	c.Assert(oc.OperatorCount(schedule.OpLeader), Equals, uint64(1)) // 1:leader
	op2 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), schedule.OpLeader)
	oc.AddOperator(op2)
	c.Assert(oc.OperatorCount(schedule.OpLeader), Equals, uint64(2)) // 1:leader, 2:leader
	oc.RemoveOperator(op1)
	c.Assert(oc.OperatorCount(schedule.OpLeader), Equals, uint64(1)) // 2:leader

	op1 = newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), schedule.OpRegion)
	oc.AddOperator(op1)
	c.Assert(oc.OperatorCount(schedule.OpRegion), Equals, uint64(1)) // 1:region 2:leader
	c.Assert(oc.OperatorCount(schedule.OpLeader), Equals, uint64(1))
	op2 = newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), schedule.OpRegion)
	op2.SetPriorityLevel(core.HighPriority)
	oc.AddOperator(op2)
	c.Assert(oc.OperatorCount(schedule.OpRegion), Equals, uint64(2)) // 1:region 2:region
	c.Assert(oc.OperatorCount(schedule.OpLeader), Equals, uint64(0))
}

var _ = Suite(&testScheduleControllerSuite{})

type testScheduleControllerSuite struct{}

// FIXME: remove after move into schedulers package
type mockLimitScheduler struct {
	schedule.Scheduler
	limit   uint64
	counter *schedule.OperatorController
	kind    schedule.OperatorKind
}

func (s *mockLimitScheduler) IsScheduleAllowed(cluster schedule.Cluster) bool {
	return s.counter.OperatorCount(s.kind) < s.limit
}

func (s *testScheduleControllerSuite) TestController(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	tc.addLeaderRegion(1, 1)
	tc.addLeaderRegion(2, 2)

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	oc := co.opController
	scheduler, err := schedule.CreateScheduler("balance-leader", oc)
	c.Assert(err, IsNil)
	lb := &mockLimitScheduler{
		Scheduler: scheduler,
		counter:   oc,
		kind:      schedule.OpLeader,
	}

	sc := newScheduleController(co, lb)

	for i := schedulers.MinScheduleInterval; sc.GetInterval() != schedulers.MaxScheduleInterval; i = sc.GetNextInterval(i) {
		c.Assert(sc.GetInterval(), Equals, i)
		c.Assert(sc.Schedule(tc.clusterInfo, schedule.NewOpInfluence(nil, tc.clusterInfo)), IsNil)
	}
	// limit = 2
	lb.limit = 2
	// count = 0
	c.Assert(sc.AllowSchedule(), IsTrue)
	op1 := newTestOperator(1, tc.GetRegion(1).GetRegionEpoch(), schedule.OpLeader)
	c.Assert(oc.AddOperator(op1), IsTrue)
	// count = 1
	c.Assert(sc.AllowSchedule(), IsTrue)
	op2 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), schedule.OpLeader)
	c.Assert(oc.AddOperator(op2), IsTrue)
	// count = 2
	c.Assert(sc.AllowSchedule(), IsFalse)
	oc.RemoveOperator(op1)
	// count = 1
	c.Assert(sc.AllowSchedule(), IsTrue)

	// add a PriorityKind operator will remove old operator
	op3 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), schedule.OpHotRegion)
	op3.SetPriorityLevel(core.HighPriority)
	c.Assert(oc.AddOperator(op1), IsTrue)
	c.Assert(sc.AllowSchedule(), IsFalse)
	c.Assert(oc.AddOperator(op3), IsTrue)
	c.Assert(sc.AllowSchedule(), IsTrue)
	oc.RemoveOperator(op3)

	// add a admin operator will remove old operator
	c.Assert(oc.AddOperator(op2), IsTrue)
	c.Assert(sc.AllowSchedule(), IsFalse)
	op4 := newTestOperator(2, tc.GetRegion(2).GetRegionEpoch(), schedule.OpAdmin)
	op4.SetPriorityLevel(core.HighPriority)
	c.Assert(oc.AddOperator(op4), IsTrue)
	c.Assert(sc.AllowSchedule(), IsTrue)
	oc.RemoveOperator(op4)

	// test wrong region id.
	op5 := newTestOperator(3, &metapb.RegionEpoch{}, schedule.OpHotRegion)
	c.Assert(oc.AddOperator(op5), IsFalse)

	// test wrong region epoch.
	oc.RemoveOperator(op1)
	epoch := &metapb.RegionEpoch{
		Version: tc.GetRegion(1).GetRegionEpoch().GetVersion() + 1,
		ConfVer: tc.GetRegion(1).GetRegionEpoch().GetConfVer(),
	}
	op6 := newTestOperator(1, epoch, schedule.OpLeader)
	c.Assert(oc.AddOperator(op6), IsFalse)
	epoch.Version--
	op6 = newTestOperator(1, epoch, schedule.OpLeader)
	c.Assert(oc.AddOperator(op6), IsTrue)
	oc.RemoveOperator(op6)
}

func (s *testScheduleControllerSuite) TestInterval(c *C) {
	_, opt := newTestScheduleConfig()
	tc := newTestClusterInfo(opt)
	hbStreams := newHeartbeatStreams(tc.getClusterID())
	defer hbStreams.Close()

	co := newCoordinator(tc.clusterInfo, hbStreams, namespace.DefaultClassifier)
	lb, err := schedule.CreateScheduler("balance-leader", co.opController)
	c.Assert(err, IsNil)
	sc := newScheduleController(co, lb)

	// If no operator for x seconds, the next check should be in x/2 seconds.
	idleSeconds := []int{5, 10, 20, 30, 60}
	for _, n := range idleSeconds {
		sc.nextInterval = schedulers.MinScheduleInterval
		for totalSleep := time.Duration(0); totalSleep <= time.Second*time.Duration(n); totalSleep += sc.GetInterval() {
			c.Assert(sc.Schedule(tc.clusterInfo, schedule.NewOpInfluence(nil, tc.clusterInfo)), IsNil)
		}
		c.Assert(sc.GetInterval(), Less, time.Second*time.Duration(n/2))
	}
}

func waitAddLearner(c *C, stream *mockHeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() &&
				res.GetChangePeer().GetChangeType() == eraftpb.ConfChangeType_AddLearnerNode &&
				res.GetChangePeer().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	return region.Clone(
		core.WithAddPeer(res.GetChangePeer().GetPeer()),
		core.WithIncConfVer(),
	)
}

func waitPromoteLearner(c *C, stream *mockHeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() &&
				res.GetChangePeer().GetChangeType() == eraftpb.ConfChangeType_AddNode &&
				res.GetChangePeer().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	// Remove learner than add voter.
	return region.Clone(
		core.WithRemoveStorePeer(storeID),
		core.WithAddPeer(res.GetChangePeer().GetPeer()),
	)
}

func waitRemovePeer(c *C, stream *mockHeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() &&
				res.GetChangePeer().GetChangeType() == eraftpb.ConfChangeType_RemoveNode &&
				res.GetChangePeer().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	return region.Clone(
		core.WithRemoveStorePeer(storeID),
		core.WithIncConfVer(),
	)
}

func waitTransferLeader(c *C, stream *mockHeartbeatStream, region *core.RegionInfo, storeID uint64) *core.RegionInfo {
	var res *pdpb.RegionHeartbeatResponse
	testutil.WaitUntil(c, func(c *C) bool {
		if res = stream.Recv(); res != nil {
			return res.GetRegionId() == region.GetID() && res.GetTransferLeader().GetPeer().GetStoreId() == storeID
		}
		return false
	})
	return region.Clone(
		core.WithLeader(res.GetTransferLeader().GetPeer()),
	)
}

func waitNoResponse(c *C, stream *mockHeartbeatStream) {
	testutil.WaitUntil(c, func(c *C) bool {
		res := stream.Recv()
		return res == nil
	})
}
