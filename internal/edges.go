package internal

import (
	"iter"
	"time"
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
	childIDs []int64
}

// groupEdges collects edges of the same kind and parent together in order to
// reduce the number of times we deserialize the parent during denormalization.
//
// If an edge isn't seen after the wait duration then we yield the last edge we
// saw.
func groupEdges(edges chan edge, wait time.Duration) iter.Seq[edge] {
	return func(yield func(edge) bool) {
		workEdges := map[int64]*edge{}
		authorEdges := map[int64]*edge{}

		// Always yield work edges before author edges, in case a work needs to
		// later be added to an author.
		send := func(yield func(edge) bool) bool {
			for k, e := range workEdges {
				if !yield(*e) {
					return false
				}
				delete(workEdges, k)
			}
			for k, e := range authorEdges {
				if !yield(*e) {
					return false
				}
				delete(authorEdges, k)
			}
			return true
		}

		for {
			select {
			case <-time.After(wait):
				if !send(yield) {
					return
				}
			case edge, ok := <-edges:
				if !ok {
					// Channel is closed.
					_ = send(yield)
					return
				}

				switch edge.kind {
				case authorEdge:
					e, ok := authorEdges[edge.parentID]
					if !ok {
						authorEdges[edge.parentID] = &edge
						continue
					}
					e.childIDs = append(e.childIDs, edge.childIDs...)
				case workEdge:
					e, ok := workEdges[edge.parentID]
					if !ok {
						workEdges[edge.parentID] = &edge
						continue
					}
					e.childIDs = append(e.childIDs, edge.childIDs...)
				}

				// Flush if our buffer is filling up too fast.
				if len(workEdges)+len(authorEdges) > 1000 {
					if !send(yield) {
						return
					}
				}
			}
		}
	}
}
