// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package indexer

import (
	"github.com/couchbase/indexing/secondary/common"
	"time"
)

type CompactionManager interface {
}

type compactionManager struct {
	logPrefix string
	config    common.Config
	supvMsgCh MsgChannel
	supvCmdCh MsgChannel
}

type compactionDaemon struct {
	quitch  chan bool
	started bool
	ticker  *time.Ticker
	msgch   MsgChannel
	config  common.Config
}

func (cd *compactionDaemon) Start() {
	if !cd.started {
		dur := time.Second * time.Duration(cd.config["interval"].Int())
		cd.ticker = time.NewTicker(dur)
		cd.started = true
		go cd.loop()
	}
}

func (cd *compactionDaemon) Stop() {
	if cd.started {
		cd.ticker.Stop()
		cd.quitch <- true
		<-cd.quitch
	}
}

func (cd *compactionDaemon) needsCompaction(is IndexStorageStats) bool {
	common.Infof("CompactionDaemon: Checking fragmentation of index instance:%v (Data:%v, Disk:%v)", is.InstId, is.Stats.DataSize, is.Stats.DiskSize)
	if uint64(is.Stats.DiskSize) > cd.config["minSize"].Uint64() {
		perc := float64(is.Stats.DiskSize-is.Stats.DataSize) * float64(100) / float64(is.Stats.DataSize+1)
		if float64(perc) >= float64(cd.config["minFrag"].Int()) {
			return true
		}
	}

	return false
}

func (cd *compactionDaemon) loop() {
	var stats []IndexStorageStats
loop:
	for {
		select {
		case _, ok := <-cd.ticker.C:
			if ok {
				replych := make(chan []IndexStorageStats)
				statReq := &MsgIndexStorageStats{respch: replych}
				cd.msgch <- statReq
				stats = <-replych

				for _, is := range stats {
					if cd.needsCompaction(is) {
						errch := make(chan error)
						compactReq := &MsgIndexCompact{
							instId: is.InstId,
							errch:  errch,
						}
						common.Infof("CompactionDaemon: Compacting index instance:%v", is.InstId)
						cd.msgch <- compactReq
						err := <-errch
						if err == nil {
							common.Infof("CompactionDaemon: Finished compacting index instance:%v", is.InstId)
						} else {
							common.Errorf("CompactionDaemon: Index instance:%v Compaction failed with reason - %v", is.InstId, err)
						}
					}
				}
			}

		case <-cd.quitch:
			cd.quitch <- true
			break loop
		}
	}
}

func NewCompactionManager(supvCmdCh MsgChannel, supvMsgCh MsgChannel,
	config common.Config) (CompactionManager, Message) {
	cm := &compactionManager{
		config:    config,
		supvCmdCh: supvCmdCh,
		supvMsgCh: supvMsgCh,
		logPrefix: "CompactionManager",
	}
	go cm.run()
	return cm, &MsgSuccess{}
}

func (cm *compactionManager) run() {
	cd := cm.newCompactionDaemon()
	cd.Start()
loop:
	for {
		select {
		case cmd, ok := <-cm.supvCmdCh:
			if ok {
				if cmd.GetMsgType() == COMPACTION_MGR_SHUTDOWN {
					common.Infof("%v: Shutting Down", cm.logPrefix)
					cm.supvCmdCh <- &MsgSuccess{}
					break loop
				}
			} else {
				break loop
			}
		}
	}

	cd.Stop()
}

func (cm *compactionManager) newCompactionDaemon() *compactionDaemon {
	cfg := cm.config.SectionConfig("compaction.", true)
	cd := &compactionDaemon{
		quitch:  make(chan bool),
		config:  cfg,
		started: false,
		msgch:   cm.supvMsgCh,
	}
	return cd
}