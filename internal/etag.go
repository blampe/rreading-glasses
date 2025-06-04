package internal

import (
	"crypto/md5"
	"encoding/hex"
	"hash"
)

// etagReader is an io.Writer that computes an MD5 etag for the contents
// written to is.
type etagger struct {
	hash.Hash
}

func newETagWriter() *etagger {
	return &etagger{Hash: md5.New()}
}

func (e *etagger) ETag() string {
	return hex.EncodeToString(e.Sum(nil))
}
