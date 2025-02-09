package main

import (
	"slices"
	"sync"
)

// edges tracks parent/child relationships, in particular author/works and
// work/editions. It's primary purpose is facilitate denormalization without
// needing repeated de-serialization of resources to determine relationships.
// It is relatively memory-efficient at the cost of O(log E) lookups and
// inserts. We gradually build this in-memory as items are fetched, and for
// tens of millions of IDs it takes only a few hundred MB.
type edges struct {
	mu sync.RWMutex
	m  map[int64][]int64
}

type edgeKind int

const (
	authorEdge edgeKind = 1
	workEdge   edgeKind = 2
)

// edge represents a parent/child relationship.
type edge struct {
	kind     edgeKind
	parentID int64
	childID  int64
}

func (e *edges) Add(parentID, childID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.m == nil {
		e.m = map[int64][]int64{}
	}

	children, ok := e.m[parentID]
	if !ok {
		children = []int64{}
		e.m[parentID] = children
	}

	idx, ok := slices.BinarySearch(children, childID)
	if ok {
		return // Already present.
	}

	e.m[parentID] = slices.Insert(children, idx, childID)
}

// Remove deletes the parent/child relationship. Appropriate for when a child
// has been modified and the parent needs a refresh.
func (e *edges) Remove(parentID, childID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.m == nil {
		e.m = map[int64][]int64{}
	}

	children, ok := e.m[parentID]
	if !ok {
		return
	}

	idx, ok := slices.BinarySearch(children, childID)
	if !ok {
		return // Already absent.
	}

	e.m[parentID] = slices.Delete(children, idx, idx+1)
}

// Contains returns true if the child already exists on the parent.
func (e *edges) Contains(parentID, childID int64) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	children, ok := e.m[parentID]
	if !ok {
		return false
	}

	_, ok = slices.BinarySearch(children, childID)
	return ok
}

// Children returns all known edges belonging to the parent, but not
// necessarily all children currently stored on the parent.
func (e *edges) Children(parentID int64) []int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()

	children := e.m[parentID]
	return children
}
