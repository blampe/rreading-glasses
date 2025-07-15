package internal

import (
	"context"
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
func groupEdges(ctx context.Context, edges chan edge, wait time.Duration) iter.Seq[edge] {
	return func(yield func(edge) bool) {
		var next edge
		var ok bool

		edge := <-edges
		for {
			select {
			case next, ok = <-edges:
				if !ok {
					// Channel is closed.
					_ = yield(edge)
					return
				}
			case <-time.After(wait):
				if !yield(edge) {
					return
				}
				// Wait until we see the next edge, then start over.
				edge = <-edges
				continue
			case <-ctx.Done():
				return
			}

			// If the next edge is for the same parent and kind, then aggregate
			// its children into ours and move on.
			if edge.parentID == next.parentID && edge.kind == next.kind {
				edge.childIDs = append(edge.childIDs, next.childIDs...)
				continue
			}

			// Next edge is for a different parent, so yield our current edge.
			if !yield(edge) {
				return
			}

			edge = next
		}
	}
}
