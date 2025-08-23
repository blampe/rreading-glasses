package internal

import (
	"fmt"
	"sync"
	"sync/atomic"
)

type bbuffer[T any] interface {
	peek() (T, bool)
	pop() T
	push(T)
	len() int
}

// accumulate reads values produced by the consumer into an in-memory buffer. A
// channel is returned which provides those buffered values for consumption.
//
// This is helpful for smoothing out spikes in activity. Without this we could
// have tens of thousands of idle goroutines, at which point the scheduler can
// eat up CPU trying to find something to run.
func accumulate[T any](producer <-chan T, buf bbuffer[T]) <-chan T {
	c := make(chan T)

	go func() {
		for {
			// If our buffer is empty our consumer<- will just no-op until
			// something is produced.
			var consumer chan T
			var next T
			if t, ok := buf.peek(); ok {
				consumer = c
				next = t
			}

			// Either buffer the next produced element, or pass a buffered
			// entry down to the consumer.
			select {
			case val, ok := <-producer:
				if !ok {
					close(c)
					return
				}
				buf.push(val)
			case consumer <- next:
				_ = buf.pop()
			}
		}
	}()

	return c
}

// slicebuffer is a simple slice buffer. It is not thread safe.
type slicebuffer[T any] []T

//nolint:unused // Linter seems confused by generics.
func (s *slicebuffer[T]) pop() T {
	ss := (*s)[0]
	*s = (*s)[1:]
	return ss
}

//nolint:unused // Linter seems confused by generics.
func (s *slicebuffer[T]) push(t T) {
	if s == nil {
		s = &slicebuffer[T]{}
	}
	*s = append(*s, t)
}

//nolint:unused // Linter seems confused by generics.
func (s *slicebuffer[T]) peek() (T, bool) {
	if s == nil || len(*s) == 0 {
		var t T
		return t, false
	}
	return (*s)[0], true
}

//nolint:unused // Linter seems confused by generics.
func (s *slicebuffer[T]) len() int {
	return len(*s)
}

// edgebuf collects and merges denormalization steps while still maintaining
// serializability.
type edgebuf struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*edge
	works   map[int64]*edge
	authors map[int64]*edge
	size    atomic.Int32
}

// push enqueues the edge. If an edge of the same kind was already
// enqueued for this parent, the children will be merged.
func (b *edgebuf) push(e edge) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var existing *edge
	var ok bool

	if b.authors == nil {
		b.authors = map[int64]*edge{}
	}
	if b.works == nil {
		b.works = map[int64]*edge{}
	}
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}

	switch e.kind {
	case authorEdge:
		existing, ok = b.authors[e.parentID]
		if !ok {
			b.authors[e.parentID] = &e
		}
	case workEdge:
		existing, ok = b.works[e.parentID]
		if !ok {
			b.works[e.parentID] = &e
		}
	case refreshDone:
		// Nothing else to do.
	default:
		panic(fmt.Sprintf("unrecognized edge kind %q", e.kind))
	}

	if ok {
		combined := union(existing.childIDs, e.childIDs)
		b.size.Add(int32(len(combined) - len(existing.childIDs)))
		existing.childIDs = combined
	} else {
		b.size.Add(int32(len(e.childIDs)))
		b.queue = append(b.queue, &e)
	}
	b.cond.Signal()
}

// peek returns the next element if there is one, or false if there isn't.
func (b *edgebuf) peek() (edge, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.queue) == 0 {
		return edge{}, false
	}
	return *b.queue[0], true
}

// pop returns the next edge in FIFO order, or blocks until an edge is
// available.
func (b *edgebuf) pop() edge {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}

	for len(b.queue) == 0 {
		b.cond.Wait()
	}

	edge := b.queue[0]
	b.queue = b.queue[1:]

	switch edge.kind {
	case authorEdge:
		delete(b.authors, edge.parentID)
	case workEdge:
		delete(b.works, edge.parentID)
	case refreshDone:
		// Nothing else to do.
	default:
		panic("unrecognized edge kind")
	}

	b.size.Add(-int32(len(edge.childIDs)))

	return *edge
}

// len returns the number of children currently waiting in the buffer.
func (b *edgebuf) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return int(b.size.Load())
}
