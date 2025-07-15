package internal

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGroupEdges(t *testing.T) {
	c := make(chan edge)
	go func() {
		c <- edge{kind: authorEdge, parentID: 100, childIDs: []int64{1}}
		c <- edge{kind: authorEdge, parentID: 100, childIDs: []int64{2, 3}}
		c <- edge{kind: workEdge, parentID: 100, childIDs: []int64{4}}
		c <- edge{kind: authorEdge, parentID: 100, childIDs: []int64{5, 6}}
		c <- edge{kind: authorEdge, parentID: 200, childIDs: []int64{7}}
		c <- edge{kind: authorEdge, parentID: 100, childIDs: []int64{8}}
		c <- edge{kind: authorEdge, parentID: 300, childIDs: []int64{9}}
		close(c)
	}()

	edges := slices.Collect(groupEdges(t.Context(), c, time.Second))

	assert.Equal(t, edges[0], edge{kind: authorEdge, parentID: 100, childIDs: []int64{1, 2, 3}})
	assert.Equal(t, edges[1], edge{kind: workEdge, parentID: 100, childIDs: []int64{4}})
	assert.Equal(t, edges[2], edge{kind: authorEdge, parentID: 100, childIDs: []int64{5, 6}})
	assert.Equal(t, edges[3], edge{kind: authorEdge, parentID: 200, childIDs: []int64{7}})
	assert.Equal(t, edges[4], edge{kind: authorEdge, parentID: 100, childIDs: []int64{8}})
	assert.Equal(t, edges[5], edge{kind: authorEdge, parentID: 300, childIDs: []int64{9}})
}
