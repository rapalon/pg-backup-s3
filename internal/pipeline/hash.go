// Package pipeline wires pg_dump, age and the object store into streaming
// backups, and orchestrates a whole backup run.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// HashReader hashes and counts everything read through it. It sits between
// the encrypted stream and the object store on backup, and between the
// object store and the decryptor on restore, so the SHA-256 in the manifest
// always describes the bytes as stored.
type HashReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

// NewHashReader wraps r.
func NewHashReader(r io.Reader) *HashReader {
	return &HashReader{r: r, h: sha256.New()}
}

func (h *HashReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.h.Write(p[:n])
		h.n += int64(n)
	}
	return n, err
}

// Sum returns the hex SHA-256 of everything read so far.
func (h *HashReader) Sum() string { return hex.EncodeToString(h.h.Sum(nil)) }

// Count returns the number of bytes read so far.
func (h *HashReader) Count() int64 { return h.n }
