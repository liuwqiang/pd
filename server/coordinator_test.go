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
	"time"

	"github.com/gogo/protobuf/proto"
	. "github.com/pingcap/check"
	raftpb "github.com/pingcap/kvproto/pkg/eraftpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
)

func newTestOperator(regionID uint64) *balanceOperator {
	return &balanceOperator{
		Region: newRegionInfo(&metapb.Region{Id: regionID}, nil),
	}
}

var _ = Suite(&testCoordinatorSuite{})

type testCoordinatorSuite struct{}

func (s *testCoordinatorSuite) TestBasic(c *C) {
	cluster := newClusterInfo(newMockIDAllocator())
	_, opt := newTestScheduleConfig()
	co := newCoordinator(cluster, opt)

	op := newTestOperator(1)
	co.addOperator(leaderKind, op)
	c.Assert(co.getOperatorCount(leaderKind), Equals, 1)
	c.Assert(co.getOperator(1).Region.GetId(), Equals, op.Region.GetId())

	// Region 1 already has an operator, cannot add another one.
	co.addOperator(storageKind, op)
	c.Assert(co.getOperatorCount(storageKind), Equals, 0)

	// Region 1 is in region cache, cannot add another one.
	co.removeOperator(op)
	co.addOperator(storageKind, op)
	c.Assert(co.getOperatorCount(storageKind), Equals, 0)

	// Delete region 1 from region cache, then we can add a new operator.
	co.regionCache.delete(1)
	co.addOperator(storageKind, op)
	c.Assert(co.getOperatorCount(storageKind), Equals, 1)
	c.Assert(co.getOperator(1).Region.GetId(), Equals, op.Region.GetId())
}

func (s *testCoordinatorSuite) TestSchedule(c *C) {
	cluster := newClusterInfo(newMockIDAllocator())
	tc := newTestClusterInfo(cluster)

	cfg, opt := newTestScheduleConfig()
	co := newCoordinator(cluster, opt)
	co.run()
	defer co.stop()

	// Transfer peer from store 4 to store 1.
	tc.addRegionStore(1, 1, 0.1)
	tc.addRegionStore(2, 2, 0.2)
	tc.addRegionStore(3, 3, 0.3)
	tc.addRegionStore(4, 4, 0.4)
	tc.addLeaderRegion(1, 2, 3, 4)

	// Transfer leader from store 1 to store 4.
	tc.updateLeaderCount(1, 4, 10)
	tc.updateLeaderCount(2, 3, 10)
	tc.updateLeaderCount(3, 2, 10)
	tc.updateLeaderCount(4, 1, 10)
	tc.addLeaderRegion(2, 1, 2, 3, 4)

	// Wait for schedule.
	time.Sleep(time.Second)
	checkTransferPeer(c, co.getOperator(1), 4, 1)
	checkTransferLeader(c, co.getOperator(2), 1, 4)

	// Transfer peer.
	region := cluster.getRegion(1)
	resp := co.dispatch(region)
	checkAddPeerResp(c, resp, 1)
	region.Peers = append(region.Peers, resp.GetChangePeer().GetPeer())
	c.Assert(co.dispatch(region), IsNil)
	resp = co.dispatch(region)
	checkRemovePeerResp(c, resp, 4)
	region.Peers = []*metapb.Peer{
		region.GetStorePeer(1),
		region.GetStorePeer(2),
		region.GetStorePeer(3),
	}
	c.Assert(co.dispatch(region), IsNil)
	c.Assert(co.getOperator(region.GetId()), IsNil)

	// Transfer leader.
	region = cluster.getRegion(2)
	resp = co.dispatch(region)
	checkTransferLeaderResp(c, resp, 4)
	region.Leader = resp.GetTransferLeader().GetPeer()
	c.Assert(co.dispatch(region), IsNil)
	c.Assert(co.getOperator(region.GetId()), IsNil)

	// Turn off normal balance.
	clonecfg := *cfg
	clonecfg.MinBalanceDiffRatio = 1
	opt.store(&clonecfg)

	// Test replica checker.
	// Peer in store 4 is down.
	tc.addLeaderRegion(4, 2, 3, 4)
	tc.setStoreDown(4)
	region = cluster.getRegion(4)
	downPeer := &pdpb.PeerStats{
		Peer:        region.GetStorePeer(4),
		DownSeconds: proto.Uint64(24 * 60 * 60),
	}
	region.DownPeers = append(region.DownPeers, downPeer)

	// Check ReplicaScheduleLimit.
	opCount := uint64(co.getOperatorCount(storageKind))
	clonecfg.ReplicaScheduleLimit = opCount
	opt.store(&clonecfg)
	c.Assert(co.dispatch(region), IsNil)
	clonecfg.ReplicaScheduleLimit = opCount + 1
	opt.store(&clonecfg)

	// Remove peer in store 4.
	resp = co.dispatch(region)
	checkRemovePeerResp(c, resp, 4)
	region.Peers = region.Peers[0 : len(region.Peers)-1]
	region.DownPeers = nil
	c.Assert(co.dispatch(region), IsNil)
	co.regionCache.delete(region.GetId())

	// Check ReplicaScheduleInterval.
	resp = co.dispatch(region)
	c.Assert(co.dispatch(region), IsNil)
	clonecfg.ReplicaScheduleInterval.Duration = 0
	opt.store(&clonecfg)

	// Add new peer in store 1.
	resp = co.dispatch(region)
	checkAddPeerResp(c, resp, 1)
	region.Peers = append(region.Peers, resp.GetChangePeer().GetPeer())
	c.Assert(co.dispatch(region), IsNil)
}

func (s *testCoordinatorSuite) TestPeerState(c *C) {
	cluster := newClusterInfo(newMockIDAllocator())
	tc := newTestClusterInfo(cluster)

	_, opt := newTestScheduleConfig()
	co := newCoordinator(cluster, opt)
	co.run()
	defer co.stop()

	// Transfer peer from store 4 to store 1.
	tc.addRegionStore(1, 1, 0.1)
	tc.addRegionStore(2, 2, 0.2)
	tc.addRegionStore(3, 3, 0.3)
	tc.addRegionStore(4, 4, 0.4)
	tc.addLeaderRegion(1, 2, 3, 4)

	// Wait for schedule.
	time.Sleep(time.Second)
	checkTransferPeer(c, co.getOperator(1), 4, 1)

	region := cluster.getRegion(1)

	// Add new peer.
	resp := co.dispatch(region)
	checkAddPeerResp(c, resp, 1)
	newPeer := resp.GetChangePeer().GetPeer()
	region.Peers = append(region.Peers, newPeer)

	// If the new peer is pending, the operator will not finish.
	region.PendingPeers = append(region.PendingPeers, newPeer)
	checkAddPeerResp(c, co.dispatch(region), 1)

	// The new peer is not pending now, the operator will finish.
	region.PendingPeers = nil
	c.Assert(co.dispatch(region), IsNil)
}

func (s *testCoordinatorSuite) TestAddScheduler(c *C) {
	cluster := newClusterInfo(newMockIDAllocator())
	tc := newTestClusterInfo(cluster)

	_, opt := newTestScheduleConfig()
	co := newCoordinator(cluster, opt)
	co.run()
	defer co.stop()

	c.Assert(co.schedulers, HasLen, 2)
	c.Assert(co.removeScheduler("leader-balancer"), IsTrue)
	c.Assert(co.removeScheduler("storage-balancer"), IsTrue)
	c.Assert(co.schedulers, HasLen, 0)

	// Add stores 1,2,3
	tc.addLeaderStore(1, 1, 1)
	tc.addLeaderStore(2, 1, 1)
	tc.addLeaderStore(3, 1, 1)
	// Add regions 1 with leader in store 1 and followers in stores 2,3
	tc.addLeaderRegion(1, 1, 2, 3)
	// Add regions 2 with leader in store 2 and followers in stores 1,3
	tc.addLeaderRegion(2, 2, 1, 3)
	// Add regions 3 with leader in store 3 and followers in stores 1,2
	tc.addLeaderRegion(3, 3, 1, 2)

	gls := newGrantLeaderScheduler(1)
	c.Assert(co.removeScheduler(gls.GetName()), IsFalse)
	c.Assert(co.addScheduler(newLeaderScheduleController(co, gls)), IsTrue)

	// Transfer all leaders to store 1.
	time.Sleep(100 * time.Millisecond)
	checkTransferLeaderResp(c, co.dispatch(cluster.getRegion(2)), 1)
	tc.addLeaderRegion(2, 1, 2, 3)
	time.Sleep(100 * time.Millisecond)
	checkTransferLeaderResp(c, co.dispatch(cluster.getRegion(3)), 1)
	tc.addLeaderRegion(3, 1, 2, 3)
	time.Sleep(100 * time.Millisecond)
	c.Assert(co.dispatch(cluster.getRegion(2)), IsNil)
	c.Assert(co.dispatch(cluster.getRegion(3)), IsNil)
}

var _ = Suite(&testControllerSuite{})

type testControllerSuite struct{}

func (s *testControllerSuite) Test(c *C) {
	cluster := newClusterInfo(newMockIDAllocator())
	cfg, opt := newTestScheduleConfig()

	cfg.LeaderScheduleLimit = 2
	co := newCoordinator(cluster, opt)
	s.test(c, co, newLeaderController(co), leaderKind)

	cfg.StorageScheduleLimit = 2
	co = newCoordinator(cluster, opt)
	s.test(c, co, newStorageController(co), storageKind)
}

func (s *testControllerSuite) test(c *C, co *coordinator, ctrl Controller, kind ResourceKind) {
	c.Assert(ctrl.AllowSchedule(), IsTrue)

	co.addOperator(kind, newTestOperator(1))
	c.Assert(ctrl.AllowSchedule(), IsTrue)

	co.addOperator(kind, newTestOperator(2))
	c.Assert(ctrl.AllowSchedule(), IsFalse)

	co.wg.Add(1)
	go func() {
		select {
		case <-ctrl.Ctx().Done():
			co.wg.Done()
		}
	}()

	co.stop()
}

func checkAddPeerResp(c *C, resp *pdpb.RegionHeartbeatResponse, storeID uint64) {
	changePeer := resp.GetChangePeer()
	c.Assert(changePeer.GetChangeType(), Equals, raftpb.ConfChangeType_AddNode)
	c.Assert(changePeer.GetPeer().GetStoreId(), Equals, storeID)
}

func checkRemovePeerResp(c *C, resp *pdpb.RegionHeartbeatResponse, storeID uint64) {
	changePeer := resp.GetChangePeer()
	c.Assert(changePeer.GetChangeType(), Equals, raftpb.ConfChangeType_RemoveNode)
	c.Assert(changePeer.GetPeer().GetStoreId(), Equals, storeID)
}

func checkTransferLeaderResp(c *C, resp *pdpb.RegionHeartbeatResponse, storeID uint64) {
	c.Assert(resp.GetTransferLeader().GetPeer().GetStoreId(), Equals, storeID)
}