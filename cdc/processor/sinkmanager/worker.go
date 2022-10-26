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
	"sync/atomic"

	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/pkg/sorter"
)

type writeSuccessCallback func(lastWrittenPos sorter.Position)

type tableSinkTask struct {
	tableID       model.TableID
	lowerBound    sorter.Position
	lastBarrierTs *atomic.Uint64
	tableSink     *tableSinkWrapper
	callback      writeSuccessCallback
}

type worker interface {
	// Pull data from source manager.
	run(taskChan <-chan *tableSinkTask) error
	// Close the worker.
	close()
}
