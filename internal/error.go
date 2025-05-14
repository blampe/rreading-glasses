package internal

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	errNotFound   = statusErr(http.StatusNotFound)
	errBadRequest = statusErr(http.StatusBadRequest)
	errTryAgain   = statusErr(http.StatusTooManyRequests) // Will be retried.

	errMissingIDs = errors.Join(fmt.Errorf(`missing "ids"`), errBadRequest)
)

type statusErr int

var _ error = (*statusErr)(nil)

func (s statusErr) Status() int {
	return int(s)
}

func (s statusErr) Error() string {
	return fmt.Sprintf("HTTP %d: %s", s, http.StatusText(int(s)))
}
