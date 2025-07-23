package internal

import (
	"iter"
	"maps"
	"sync"
)

type edgeKind int

const (
	authorEdge edgeKind = 1
	workEdge   edgeKind = 2
)

// edge represents a parent/child relationship.
type edge struct {
	kind     edgeKind
	parentID int64
	childIDs set[int64]
}

// groupEdges collects edges of the same kind and parent together in order to
// reduce the number of times we deserialize the parent during denormalization.
//
// If an edge isn't seen after the wait duration then we yield the last edge we
// saw.
func groupEdges(edges chan edge) iter.Seq[edge] {
	buffer := &edgebuf{
		queue:   []*edge{},
		works:   map[int64]*edge{},
		authors: map[int64]*edge{},
	}
	buffer.cond = sync.NewCond(&buffer.mu)

	go func() {
		for e := range edges {
			buffer.push(&e)
		}
		buffer.close()
	}()

	return func(yield func(edge) bool) {
		for {
			edge, ok := buffer.pop()
			if !ok {
				return
			}
			if !yield(*edge) {
				return
			}
		}
	}
}

type set[T comparable] map[T]struct{}

func newSet[T comparable](ts ...T) set[T] {
	s := set[T]{}
	for _, t := range ts {
		s[t] = struct{}{}
	}
	return s
}

func union[T comparable, S set[T]](x S, y S) S {
	r := maps.Clone(x)
	maps.Copy(r, y)
	return r
}

type edgebuf struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*edge
	works   map[int64]*edge
	authors map[int64]*edge

	closed bool
}

func (b *edgebuf) push(e *edge) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var existing *edge
	var ok bool

	switch e.kind {
	case authorEdge:
		existing, ok = b.authors[e.parentID]
		if !ok {
			b.authors[e.parentID] = e
		}
	case workEdge:
		existing, ok = b.works[e.parentID]
		if !ok {
			b.works[e.parentID] = e
		}
	}
	if ok {
		existing.childIDs = union(existing.childIDs, e.childIDs)
	} else {
		b.queue = append(b.queue, e)
	}
	b.cond.Signal()
}

func (b *edgebuf) pop() (*edge, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for len(b.queue) == 0 && !b.closed {
		b.cond.Wait()
	}

	if len(b.queue) == 0 {
		return nil, false
	}

	edge := b.queue[0]
	b.queue = b.queue[1:]

	switch edge.kind {
	case authorEdge:
		delete(b.authors, edge.parentID)
	case workEdge:
		delete(b.works, edge.parentID)
	}

	return edge, true
}

func (b *edgebuf) close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed = true
	b.cond.Broadcast()
}
