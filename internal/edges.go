package internal

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
