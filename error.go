package main

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	errNotFound       = statusErr(http.StatusNotFound) //nolint:unused
	errBadRequest     = statusErr(http.StatusBadRequest)
	errNotImplemented = statusErr(http.StatusNotImplemented)

	errMissingIDs = errors.Join(fmt.Errorf(`missing "ids"`), errBadRequest)
)

type statusErr int

var _ error = (*statusErr)(nil)

func (s statusErr) Status() int {
	return int(s)
}

func (s statusErr) Error() string {
	return fmt.Sprintf("HTTP %d", s)
}
