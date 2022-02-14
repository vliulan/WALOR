// Copyright 2017 PingCAP, Inc.
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

package schedulers

import (
	"strconv"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/pd/server/cache"
	"github.com/pingcap/pd/server/core"
	"github.com/pingcap/pd/server/schedule"
	log "github.com/sirupsen/logrus"
)

func init() {
	schedule.RegisterScheduler("balance-region", func(limiter *schedule.Limiter, args []string) (schedule.Scheduler, error) {
		return newBalanceRegionScheduler(limiter), nil
	})
}

// balanceRegionRetryLimit is the limit to retry schedule for selected store.
const balanceRegionRetryLimit = 10

type balanceRegionScheduler struct {
	*baseScheduler
	selector    schedule.Selector
	taintStores *cache.TTLUint64
}

// newBalanceRegionScheduler creates a scheduler that tends to keep regions on
// each store balanced.
func newBalanceRegionScheduler(limiter *schedule.Limiter) schedule.Scheduler {
	taintStores := newTaintCache()
	filters := []schedule.Filter{
		schedule.NewCacheFilter(taintStores),
		schedule.NewStateFilter(),
		schedule.NewHealthFilter(),
		schedule.NewSnapshotCountFilter(),
		schedule.NewStorageThresholdFilter(),
		schedule.NewPendingPeerCountFilter(),
	}
	base := newBaseScheduler(limiter)
	return &balanceRegionScheduler{
		baseScheduler: base,
		selector:      schedule.NewBalanceSelector(core.RegionKind, filters),
		taintStores:   taintStores,
	}
}

func (s *balanceRegionScheduler) GetName() string {
	return "balance-region-scheduler"
}

func (s *balanceRegionScheduler) GetType() string {
	return "balance-region"
}

func (s *balanceRegionScheduler) IsScheduleAllowed(cluster schedule.Cluster) bool {
	return s.limiter.OperatorCount(schedule.OpRegion) < cluster.GetRegionScheduleLimit()
}

func (s *balanceRegionScheduler) Schedule(cluster schedule.Cluster, opInfluence schedule.OpInfluence) []*schedule.Operator {
	schedulerCounter.WithLabelValues(s.GetName(), "schedule").Inc()

	stores := cluster.GetStores()

	// source is the store with highest leade score in the list that can be selected as balance source.
	source := s.selector.SelectSource(cluster, stores)
	if source == nil {
		schedulerCounter.WithLabelValues(s.GetName(), "no_store").Inc()
		// When the cluster is balanced, all stores will be added to the cache once
		// all of them have been selected. This will cause the scheduler to not adapt
		// to sudden change of a store's leader. Here we clear the taint cache and
		// re-iterate.
		s.taintStores.Clear()
		return nil
	}

	log.Debugf("[%s] store%d has the max region score", s.GetName(), source.GetId())
	sourceLabel := strconv.FormatUint(source.GetId(), 10)
	balanceRegionCounter.WithLabelValues("source_store", sourceLabel).Inc()

	for i := 0; i < balanceRegionRetryLimit; i++ {
		region := cluster.RandFollowerRegion(source.GetId())
		if region == nil {
			region = cluster.RandLeaderRegion(source.GetId())
		}
		if region == nil {
			schedulerCounter.WithLabelValues(s.GetName(), "no_region").Inc()
			continue
		}
		log.Debugf("[%s] select region%d", s.GetName(), region.GetId())

		// We don't schedule region with abnormal number of replicas.
		if len(region.GetPeers()) != cluster.GetMaxReplicas() {
			log.Debugf("[%s] region%d has abnormal replica count", s.GetName(), region.GetId())
			schedulerCounter.WithLabelValues(s.GetName(), "abnormal_replica").Inc()
			continue
		}

		// Skip hot regions.
		if cluster.IsRegionHot(region.GetId()) {
			//log.Infof("[%s] region%d is hot", s.GetName(), region.GetId())
			log.Debugf("[%s] region%d is hot", s.GetName(), region.GetId())
			schedulerCounter.WithLabelValues(s.GetName(), "region_hot").Inc()
			continue
		}

		oldPeer := region.GetStorePeer(source.GetId())
		if op := s.transferPeer(cluster, region, oldPeer, opInfluence); op != nil {
			schedulerCounter.WithLabelValues(s.GetName(), "new_operator").Inc()
			return []*schedule.Operator{op}
		}
	}

	// If no operator can be created for the selected store, ignore it for a while.
	log.Debugf("[%s] no operator created for selected store%d", s.GetName(), source.GetId())
	log.Debugf("[%s] no operator created for selected store%d", s.GetName(), source.GetId())
	balanceRegionCounter.WithLabelValues("add_taint", sourceLabel).Inc()
	s.taintStores.Put(source.GetId())
	return nil
}

func (s *balanceRegionScheduler) transferPeer(cluster schedule.Cluster, region *core.RegionInfo, oldPeer *metapb.Peer, opInfluence schedule.OpInfluence) *schedule.Operator {
	// scoreGuard guarantees that the distinct score will not decrease.
	stores := cluster.GetRegionStores(region)
	source := cluster.GetStore(oldPeer.GetStoreId())
	scoreGuard := schedule.NewDistinctScoreFilter(cluster.GetLocationLabels(), stores, source)
	
	//log.Infof("region: %d, stores: %v, source: %v, scoreGuard: %v", region.GetId(), stores, source, scoreGuard)	// wyy add
	
	checker := schedule.NewReplicaChecker(cluster, nil)
	newPeer := checker.SelectBestReplacedPeerToAddReplica(region, oldPeer, scoreGuard)
	if newPeer == nil {
		schedulerCounter.WithLabelValues(s.GetName(), "no_peer").Inc()
		return nil
	}

	target := cluster.GetStore(newPeer.GetStoreId())
	//log.Infof("[region %d] source store id is %v, target store id is %v", region.GetId(), source.GetId(), target.GetId())
	log.Debugf("[region %d] source store id is %v, target store id is %v", region.GetId(), source.GetId(), target.GetId())

	sourceSize := source.RegionSize + int64(opInfluence.GetStoreInfluence(source.GetId()).RegionSize)
	targetSize := target.RegionSize + int64(opInfluence.GetStoreInfluence(target.GetId()).RegionSize)
	regionSize := float64(region.ApproximateSize) * cluster.GetTolerantSizeRatio()
	if !shouldBalance(sourceSize, source.RegionWeight, targetSize, target.RegionWeight, regionSize) {
		log.Debugf("[%s] skip balance region%d, source size: %v, source weight: %v, target size: %v, target weight: %v, region size: %v", s.GetName(), region.GetId(), sourceSize, source.RegionWeight, targetSize, target.RegionWeight, region.ApproximateSize)
		schedulerCounter.WithLabelValues(s.GetName(), "skip").Inc()
		return nil
	}
	
	// wyy add
	if (sourceSize < 6144 && source.RegionWeight == 1e-6) {
		schedulerCounter.WithLabelValues(s.GetName(), "skip").Inc()
		return nil
	}

	return schedule.CreateMovePeerOperator("balance-region", cluster, region, schedule.OpBalance, oldPeer.GetStoreId(), newPeer.GetStoreId(), newPeer.GetId())
}
