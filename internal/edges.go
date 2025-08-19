package internal

type edgeKind int

const (
	authorEdge  edgeKind = 1
	workEdge    edgeKind = 2
	refreshDone edgeKind = 3
)

// edge represents a parent/child relationship. They are used for denormalizing
// children to parent objects (works to authors, editions to works).
type edge struct {
	kind     edgeKind
	parentID int64
	childIDs set[int64]
}
