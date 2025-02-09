package main

import (
	"testing"

	"github.com/DmitriyVTitov/size"
	"github.com/stretchr/testify/assert"
)

func TestCommutivity(t *testing.T) {
	parent := int64(4178)
	child1 := int64(6288)
	child2 := int64(365990)

	e := edges{}

	assert.False(t, e.Contains(parent, child1))
	assert.False(t, e.Contains(child1, parent))

	e.Add(parent, child1)

	assert.True(t, e.Contains(parent, child1))
	assert.False(t, e.Contains(child1, parent))

	s := size.Of(e.m)

	// Adding a new child should only add an additional 64 bits to our memory
	// usage.
	e.Add(parent, child2)
	assert.Equal(t, s+8, size.Of(e.m))
}
