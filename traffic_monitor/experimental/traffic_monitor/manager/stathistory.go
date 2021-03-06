package manager

/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

import (
	"time"

	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/common/log"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/cache"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/config"
	ds "github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/deliveryservice"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/enum"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/health"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/peer"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/threadsafe"
	todata "github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/trafficopsdata"
	to "github.com/apache/incubator-trafficcontrol/traffic_ops/client"
)

func pruneHistory(history []cache.Result, limit uint64) []cache.Result {
	if uint64(len(history)) > limit {
		history = history[:limit-1]
	}
	return history
}

func getNewCaches(localStates peer.CRStatesThreadsafe, monitorConfigTS TrafficMonitorConfigMapThreadsafe) map[enum.CacheName]struct{} {
	monitorConfig := monitorConfigTS.Get()
	caches := map[enum.CacheName]struct{}{}
	for cacheName := range localStates.GetCaches() {
		// ONLINE and OFFLINE caches are not polled.
		// TODO add a function IsPolled() which can be called by this and the monitorConfig func which sets the polling, to prevent updating in one place breaking the other.
		if ts, ok := monitorConfig.TrafficServer[string(cacheName)]; !ok || ts.Status == "ONLINE" || ts.Status == "OFFLINE" {
			continue
		}
		caches[cacheName] = struct{}{}
	}
	return caches
}

// StartStatHistoryManager fetches the full statistics data from ATS Astats. This includes everything needed for all calculations, such as Delivery Services. This is expensive, though, and may be hard on ATS, so it should poll less often.
// For a fast 'is it alive' poll, use the Health Result Manager poll.
// Returns the stat history, the duration between the stat poll for each cache, the last Kbps data, the calculated Delivery Service stats, and the unpolled caches list.
func StartStatHistoryManager(
	cacheStatChan <-chan cache.Result,
	localStates peer.CRStatesThreadsafe,
	combinedStates peer.CRStatesThreadsafe,
	toData todata.TODataThreadsafe,
	cachesChanged <-chan struct{},
	errorCount threadsafe.Uint,
	cfg config.Config,
	monitorConfig TrafficMonitorConfigMapThreadsafe,
) (threadsafe.ResultInfoHistory, threadsafe.ResultStatHistory, threadsafe.CacheKbpses, DurationMapThreadsafe, threadsafe.LastStats, threadsafe.DSStatsReader, threadsafe.UnpolledCaches) {
	statInfoHistory := threadsafe.NewResultInfoHistory()
	statResultHistory := threadsafe.NewResultStatHistory()
	statMaxKbpses := threadsafe.NewCacheKbpses()
	lastStatDurations := NewDurationMapThreadsafe()
	lastStatEndTimes := map[enum.CacheName]time.Time{}
	lastStats := threadsafe.NewLastStats()
	dsStats := threadsafe.NewDSStats()
	unpolledCaches := threadsafe.NewUnpolledCaches()
	tickInterval := cfg.StatFlushInterval

	precomputedData := map[enum.CacheName]cache.PrecomputedData{}
	lastResults := map[enum.CacheName]cache.Result{}

	process := func(results []cache.Result) {
		processStatResults(results, statInfoHistory, statResultHistory, statMaxKbpses, combinedStates.Get(), lastStats, toData.Get(), errorCount, dsStats, lastStatEndTimes, lastStatDurations, unpolledCaches, monitorConfig.Get(), precomputedData, lastResults)
	}

	go func() {
		var ticker *time.Ticker
		<-cachesChanged // wait for the signal that localStates have been set
		unpolledCaches.SetNewCaches(getNewCaches(localStates, monitorConfig))

		for {
			var results []cache.Result
			results = append(results, <-cacheStatChan)
			if ticker != nil {
				ticker.Stop()
			}
			ticker = time.NewTicker(tickInterval)
		innerLoop:
			for {
				select {
				case <-cachesChanged:
					unpolledCaches.SetNewCaches(getNewCaches(localStates, monitorConfig))
				case <-ticker.C:
					log.Warnf("StatHistoryManager flushing queued results\n")
					process(results)
					break innerLoop
				default:
					select {
					case r := <-cacheStatChan:
						results = append(results, r)
					default:
						process(results)
						break innerLoop
					}
				}
			}
		}
	}()
	return statInfoHistory, statResultHistory, statMaxKbpses, lastStatDurations, lastStats, &dsStats, unpolledCaches
}

// processStatResults processes the given results, creating and setting DSStats, LastStats, and other stats. Note this is NOT threadsafe, and MUST NOT be called from multiple threads.
func processStatResults(
	results []cache.Result,
	statInfoHistoryThreadsafe threadsafe.ResultInfoHistory,
	statResultHistoryThreadsafe threadsafe.ResultStatHistory,
	statMaxKbpsesThreadsafe threadsafe.CacheKbpses,
	combinedStates peer.Crstates,
	lastStats threadsafe.LastStats,
	toData todata.TOData,
	errorCount threadsafe.Uint,
	dsStats threadsafe.DSStats,
	lastStatEndTimes map[enum.CacheName]time.Time,
	lastStatDurationsThreadsafe DurationMapThreadsafe,
	unpolledCaches threadsafe.UnpolledCaches,
	mc to.TrafficMonitorConfigMap,
	precomputedData map[enum.CacheName]cache.PrecomputedData,
	lastResults map[enum.CacheName]cache.Result,
) {

	// setting the statHistory could be put in a goroutine concurrent with `ds.CreateStats`, if it were slow
	statInfoHistory := statInfoHistoryThreadsafe.Get().Copy()
	statResultHistory := statResultHistoryThreadsafe.Get().Copy()
	statMaxKbpses := statMaxKbpsesThreadsafe.Get().Copy()

	for _, result := range results {
		maxStats := uint64(mc.Profile[mc.TrafficServer[string(result.ID)].Profile].Parameters.HistoryCount)
		if maxStats < 1 {
			log.Warnf("processStatResults got history count %v for %v, setting to 1\n", maxStats, result.ID)
			maxStats = 1
		}

		// TODO determine if we want to add results with errors, or just print the errors now and don't add them.
		if lastResult, ok := lastResults[result.ID]; ok {
			health.GetVitals(&result, &lastResult, &mc) // TODO precompute
		}
		statInfoHistory.Add(result, maxStats)
		statResultHistory.Add(result, maxStats)
		// Don't add errored maxes or precomputed DSStats
		if result.Error == nil {
			// max and precomputed always contain the latest result from each cache
			statMaxKbpses.AddMax(result)
			// if we failed to compute the OutBytes, keep the outbytes of the last result.
			if result.PrecomputedData.OutBytes == 0 {
				result.PrecomputedData.OutBytes = precomputedData[result.ID].OutBytes
			}
			precomputedData[result.ID] = result.PrecomputedData

		}
		lastResults[result.ID] = result
	}
	statInfoHistoryThreadsafe.Set(statInfoHistory)
	statResultHistoryThreadsafe.Set(statResultHistory)
	statMaxKbpsesThreadsafe.Set(statMaxKbpses)

	for _, result := range results {
		log.Debugf("poll %v %v CreateStats start\n", result.PollID, time.Now())
	}

	newDsStats, newLastStats, err := ds.CreateStats(precomputedData, toData, combinedStates, lastStats.Get().Copy(), time.Now())

	for _, result := range results {
		log.Debugf("poll %v %v CreateStats end\n", result.PollID, time.Now())
	}

	if err != nil {
		errorCount.Inc()
		log.Errorf("getting deliveryservice: %v\n", err)
	} else {
		dsStats.Set(newDsStats)
		lastStats.Set(newLastStats)
	}

	endTime := time.Now()
	lastStatDurations := lastStatDurationsThreadsafe.Get().Copy()
	for _, result := range results {
		if lastStatStart, ok := lastStatEndTimes[result.ID]; ok {
			d := time.Since(lastStatStart)
			lastStatDurations[result.ID] = d
		}
		lastStatEndTimes[result.ID] = endTime

		// log.Debugf("poll %v %v statfinish\n", result.PollID, endTime)
		result.PollFinished <- result.PollID
	}
	lastStatDurationsThreadsafe.Set(lastStatDurations)
	unpolledCaches.SetPolled(results, lastStats.Get())
}
