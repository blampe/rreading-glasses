package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestError(t *testing.T) {
	err := errors.Join(fmt.Errorf("invalid request"), errBadRequest)

	var s statusErr
	ok := errors.As(err, &s)
	assert.True(t, ok)

	assert.ErrorContains(t, err, "invalid request")
}
