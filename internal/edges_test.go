package internal

import (
	"iter"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

func TestGroupEdges(t *testing.T) {
	c := make(chan edge)

	grouper := grouper{metrics: newControllerMetrics(prometheus.NewRegistry())}
	pull, _ := iter.Pull(grouper.group(c))

	c <- edge{kind: authorEdge, parentID: 100, childIDs: newSet(int64(1))}
	c <- edge{kind: authorEdge, parentID: 100, childIDs: newSet(int64(2), int64(3))}
	c <- edge{kind: workEdge, parentID: 100, childIDs: newSet(int64(4))}
	e, _ := pull()
	assert.Equal(t, edge{kind: authorEdge, parentID: 100, childIDs: newSet(int64(1), int64(2), int64(3))}, e)
	e, _ = pull()
	assert.Equal(t, edge{kind: workEdge, parentID: 100, childIDs: newSet(int64(4))}, e)

	c <- edge{kind: authorEdge, parentID: 100, childIDs: newSet(int64(5), int64(6))}
	c <- edge{kind: authorEdge, parentID: 200, childIDs: newSet(int64(7))}
	c <- edge{kind: authorEdge, parentID: 100, childIDs: newSet(int64(8))}
	c <- edge{kind: authorEdge, parentID: 300, childIDs: newSet(int64(9))}
	e, _ = pull()
	assert.Equal(t, edge{kind: authorEdge, parentID: 100, childIDs: newSet(int64(5), int64(6), int64(8))}, e)
	e, _ = pull()
	assert.Equal(t, edge{kind: authorEdge, parentID: 200, childIDs: newSet(int64(7))}, e)
	e, _ = pull()
	assert.Equal(t, edge{kind: authorEdge, parentID: 300, childIDs: newSet(int64(9))}, e)

	close(c)

	_, ok := pull()
	assert.False(t, ok)
}
