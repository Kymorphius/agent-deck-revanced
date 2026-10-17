// matrix/hardened: a bounded stream keeps native pipes drained independently
// of clients, including shutdown frames emitted by ClosePseudoConsole.
package conpty

import (
	"bytes"
	"io"
	"sync"
)

const outputBacklogLimit = 8 * 1024 * 1024

type outputBuffer struct {
	mu    sync.Mutex
	ready *sync.Cond
	data  bytes.Buffer
	limit int
	done  bool
	err   error
}

func newOutputBuffer(limit int) *outputBuffer {
	buffer := &outputBuffer{limit: limit}
	buffer.ready = sync.NewCond(&buffer.mu)
	return buffer
}

func (buffer *outputBuffer) append(data []byte) error {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.done {
		return buffer.err
	}
	if len(data) > buffer.limit-buffer.data.Len() {
		buffer.done, buffer.err = true, ErrOutputBacklog
		buffer.ready.Broadcast()
		return buffer.err
	}
	_, _ = buffer.data.Write(data)
	buffer.ready.Signal()
	return nil
}

func (buffer *outputBuffer) finish(err error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if !buffer.done {
		buffer.done = true
		buffer.err = err
		if buffer.err == nil {
			buffer.err = io.EOF
		}
	}
	buffer.ready.Broadcast()
}

func (buffer *outputBuffer) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	for buffer.data.Len() == 0 && !buffer.done {
		buffer.ready.Wait()
	}
	if buffer.data.Len() > 0 {
		return buffer.data.Read(data)
	}
	return 0, buffer.err
}
