// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"sync"
)

type encJob struct {
}

type jobState struct {
	jobSize     int
	overlapSize int
	filling     []byte // accumulates input up to jobSize
	nextPrefix  []byte // overlap prefix prepared for the next dispatched job

	jobSeq int // next job sequence number

	jobCh    chan *encJob // dispatch to workers
	resultCh chan *encJob // ordered results to flusher

	workerWg  sync.WaitGroup
	flusherWg sync.WaitGroup

	flushedSeq int // last flushed sequence number

	flusherErr error
	started    bool

	overlapPool sync.Pool // *[]byte buffers for overlap prefixes
}

func (js *jobState) putOverlapBuf(b []byte) {
	if cap(b) > 0 {
		b = b[:0]
		js.overlapPool.Put(&b)
	}
}

func (e *Encoder) shutdownJobWorkers() {
	js := &e.state.jobs
	if !js.started {
		return
	}
	close(js.jobCh)
	js.workerWg.Wait()
	close(js.resultCh)
	js.flusherWg.Wait()
	js.started = false
}
