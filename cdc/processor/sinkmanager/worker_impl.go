// Copyright 2022 PingCAP, Inc.
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

package sinkmanager

import (
	"context"

	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/redo"
	"github.com/pingcap/tiflow/pkg/sorter"
)

const maxUpdateIntervalSize = 256 * 1024 * 1024

// Assert that workerImpl implements worker.
var _ worker = (*workerImpl)(nil)

type workerImpl struct {
	parentCtx   context.Context
	redoManager redo.LogManager
	sortEngine  sorter.EventSortEngine
	memQuota    memQuota
	splitTxn    bool
	batchSize   uint64
	closedChan  chan struct{}
}

func (w *workerImpl) run(taskChan <-chan *tableSinkTask) error {
	for {
		select {
		case <-w.closedChan:
			return nil
		case t := <-taskChan:
			// First time to run the task, we have initialized memory quota for the table.
			availableMem := defaultMemoryUsage
			events := make([]*model.PolymorphicEvent, 0, 1024)
			var lastPos sorter.Position
			lastTotalSize := uint64(0)
			batchCount := 0
			currentBarrierTs := t.lastBarrierTs.Load()
			upperBound := sorter.Position{
				CommitTs: currentBarrierTs - 1,
				StartTs:  currentBarrierTs,
			}
			iter := w.sortEngine.FetchByTable(t.tableID, t.lowerBound, upperBound)
			for {
				e, pos, err := iter.Next()
				if err != nil {
					return err
				}
				// There is no more data.
				if e == nil {
					break
				}
				for availableMem-e.Row.ApproximateBytes() < 0 {
					w.memQuota.ForceAcquire(defaultMemoryUsage)
					availableMem += defaultMemoryUsage
				}
				availableMem -= e.Row.ApproximateBytes()
				events = append(events, e)
				// We meet a finished transaction.
				if pos.Valid() {
					lastPos = pos
					if w.splitTxn {
						w.memQuota.ResetBatchID(t.tableID)
					}
					// Always emit the events to the sink.
					// Whatever splitTxn is true or false, we should emit the events to the sink as soon as possible.
					size, err := w.emitEventsToTableSink(t, events)
					if err != nil {
						return err
					}
					lastTotalSize += size
					events = events[:0]
					if lastTotalSize >= maxUpdateIntervalSize {
						err := w.updateTableSinkResolvedTs(t, e.CRTs, lastTotalSize)
						if err != nil {
							return err
						}
						lastTotalSize = 0
					}
					// If we exceed the whole memory quota, we should stop the task.
					// And just wait for the next round.
					if w.memQuota.IsExceed() {
						err := w.updateTableSinkResolvedTs(t, e.CRTs, lastTotalSize)
						if err != nil {
							return err
						}
						lastTotalSize = 0
						break
					}
				}
				// If we enable splitTxn, we should emit the events to the sink when the batch size is exceeded.
				if w.splitTxn && uint64(batchCount) >= w.batchSize {
					size, err := w.emitEventsToTableSink(t, events)
					if err != nil {
						return err
					}
					lastTotalSize += size
					if lastTotalSize >= maxUpdateIntervalSize {
						err := w.updateTableSinkResolvedTs(t, e.CRTs, lastTotalSize)
						if err != nil {
							return err
						}
						lastTotalSize = 0
					}
					events = events[:0]
				}
			}
			// Do not forget to refund the useless memory quota.
			w.memQuota.Refund(uint64(availableMem))
			// Add table back.
			t.callback(lastPos)
			if err := iter.Close(); err != nil {
				return err
			}
		}
	}
}

func (w *workerImpl) emitEventsToTableSink(t *tableSinkTask, events []*model.PolymorphicEvent) (uint64, error) {
	rowChangeEvents := make([]*model.RowChangedEvent, 0, len(events))
	size := 0
	for _, e := range events {
		size += e.Row.ApproximateBytes()
		rows, err := t.tableSink.verifyAndTrySplitEvent(e)
		if err != nil {
			return 0, err
		}
		rowChangeEvents = append(rowChangeEvents, rows...)
	}

	t.tableSink.emitRowChangedEvent(rowChangeEvents...)
	return uint64(size), nil
}

func (w *workerImpl) updateTableSinkResolvedTs(t *tableSinkTask, commitTs model.Ts, size uint64) error {
	resolvedTs := model.NewResolvedTs(commitTs)
	if w.splitTxn {
		resolvedTs.Mode = model.BatchResolvedMode
		resolvedTs.BatchID = w.memQuota.AllocateBatchID(t.tableID)
	}
	w.memQuota.Record(t.tableID, resolvedTs, size)
	return t.tableSink.updateTableSinkResolvedTs(resolvedTs)
}

func (w *workerImpl) close() {
	close(w.closedChan)
}
