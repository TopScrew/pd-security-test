// Copyright 2022 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"time"

	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/syncutil"
	"github.com/tikv/pd/server/core"
	"go.uber.org/zap"
)

type prepareChecker struct {
	syncutil.RWMutex
	start    time.Time
	prepared bool
}

func newPrepareChecker() *prepareChecker {
	return &prepareChecker{
		start: time.Now(),
	}
}

// Before starting up the scheduler, we need to take the proportion of the regions on each store into consideration.
func (checker *prepareChecker) check(c *core.BasicCluster) bool {
	checker.Lock()
	defer checker.Unlock()
	if checker.prepared {
		return true
	}
	if time.Since(checker.start) > collectTimeout {
		checker.prepared = true
		return true
	}
	notLoadedFromRegionsCnt := c.GetClusterNotFromStorageRegionsCnt()
	totalRegionsCnt := c.GetRegionCount()
	// The number of active regions should be more than total region of all stores * collectFactor
	if float64(totalRegionsCnt)*collectFactor > float64(notLoadedFromRegionsCnt) {
		return false
	}
	for _, store := range c.GetStores() {
		if !store.IsPreparing() && !store.IsServing() {
			continue
		}
		storeID := store.GetID()
		// For each store, the number of active regions should be more than total region of the store * collectFactor
		if float64(c.GetStoreRegionCount(storeID))*collectFactor > float64(c.GetNotFromStorageRegionsCntByStore(storeID)) {
			return false
		}
	}
	log.Info("not loaded from storage region number is satisfied, finish prepare checker", zap.Int("not-from-storage-region", notLoadedFromRegionsCnt), zap.Int("total-region", totalRegionsCnt))
	checker.prepared = true
	return true
}

func (checker *prepareChecker) isPrepared() bool {
	checker.RLock()
	defer checker.RUnlock()
	return checker.prepared
}
