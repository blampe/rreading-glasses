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
		c <- edge{kind: workEdge, parentID: 1000, childIDs: []int64{4}}
		c <- edge{kind: authorEdge, parentID: 100, childIDs: []int64{5, 6}}
		c <- edge{kind: authorEdge, parentID: 200, childIDs: []int64{7}}
		c <- edge{kind: authorEdge, parentID: 100, childIDs: []int64{8}}
		c <- edge{kind: authorEdge, parentID: 300, childIDs: []int64{9}}
		c <- edge{kind: workEdge, parentID: 1000, childIDs: []int64{10}}
		close(c)
	}()

	edges := slices.Collect(groupEdges(c, time.Second))

	assert.Equal(t, edge{kind: workEdge, parentID: 1000, childIDs: []int64{4, 10}}, edges[0])

	expected := []edge{
		{kind: authorEdge, parentID: 100, childIDs: []int64{1, 2, 3, 5, 6, 8}},
		{kind: authorEdge, parentID: 200, childIDs: []int64{7}},
		{kind: authorEdge, parentID: 300, childIDs: []int64{9}},
	}

	assert.ElementsMatch(t, expected, edges[1:])
}
