package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory ObjectStore for tests, with injectable failures.
// It mimics S3 semantics that matter to callers: an object appears only
// after a successful Put, metadata keys are lowercased, deleting a missing
// key is fine, List is sorted and recursive.
type MemStore struct {
	mu      sync.Mutex
	objects map[string]memObject
	now     func() time.Time

	// Failure injection. Each applies to every call until cleared.
	FailPutAfterBytes int64 // >0: Put fails once this many bytes were read
	FailPut           error // Put fails before reading anything
	FailGet           error
	FailStat          error
	FailList          error
	FailDelete        error
	StatSizeOffset    int64 // added to the size Stat reports (integrity tests)

	// Counters for assertions.
	Puts, Gets, Stats, Lists, Deletes int
}

type memObject struct {
	data     []byte
	modified time.Time
	metadata map[string]string
	etag     string
}

// NewMem returns an empty MemStore.
func NewMem() *MemStore {
	return &MemStore{objects: map[string]memObject{}, now: time.Now}
}

// Put implements ObjectStore.
func (m *MemStore) Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (PutResult, error) {
	m.mu.Lock()
	m.Puts++
	failPut, limit := m.FailPut, m.FailPutAfterBytes
	m.mu.Unlock()
	if failPut != nil {
		return PutResult{}, fmt.Errorf("put %s: %w", key, failPut)
	}
	if err := ctx.Err(); err != nil {
		return PutResult{}, fmt.Errorf("put %s: %w", key, err)
	}

	var buf bytes.Buffer
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		buf.Write(chunk[:n])
		if limit > 0 && int64(buf.Len()) > limit {
			return PutResult{}, fmt.Errorf("put %s: injected failure after %d bytes", key, limit)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return PutResult{}, fmt.Errorf("put %s: %w", key, err)
		}
		if err := ctx.Err(); err != nil {
			return PutResult{}, fmt.Errorf("put %s: %w", key, err)
		}
	}

	sum := sha256.Sum256(buf.Bytes())
	obj := memObject{
		data:     buf.Bytes(),
		modified: m.now(),
		metadata: lowerKeys(opts.Metadata),
		etag:     hex.EncodeToString(sum[:16]),
	}
	m.mu.Lock()
	m.objects[key] = obj
	m.mu.Unlock()
	return PutResult{Size: int64(len(obj.data)), ETag: obj.etag}, nil
}

// Get implements ObjectStore.
func (m *MemStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Gets++
	if m.FailGet != nil {
		return nil, ObjectInfo{}, fmt.Errorf("get %s: %w", key, m.FailGet)
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, ObjectInfo{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
	}
	data := append([]byte(nil), obj.data...)
	return io.NopCloser(bytes.NewReader(data)), obj.info(key), nil
}

// Stat implements ObjectStore.
func (m *MemStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Stats++
	if m.FailStat != nil {
		return ObjectInfo{}, fmt.Errorf("stat %s: %w", key, m.FailStat)
	}
	obj, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, fmt.Errorf("stat %s: %w", key, ErrNotFound)
	}
	info := obj.info(key)
	info.Size += m.StatSizeOffset
	return info, nil
}

// List implements ObjectStore.
func (m *MemStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Lists++
	if m.FailList != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, m.FailList)
	}
	var infos []ObjectInfo
	for k, obj := range m.objects {
		if strings.HasPrefix(k, prefix) {
			info := obj.info(k)
			info.Metadata = nil // List never carries metadata, like S3
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	return infos, nil
}

// Delete implements ObjectStore.
func (m *MemStore) Delete(ctx context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Deletes++
	if m.FailDelete != nil {
		return fmt.Errorf("delete: %w", m.FailDelete)
	}
	for _, k := range keys {
		delete(m.objects, k)
	}
	return nil
}

// Keys returns every stored key, sorted. Test helper.
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Bytes returns a copy of an object's content, or nil. Test helper.
func (m *MemStore) Bytes(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil
	}
	return append([]byte(nil), obj.data...)
}

// Corrupt flips one byte of a stored object. Test helper for integrity tests.
func (m *MemStore) Corrupt(key string, offset int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok || offset < 0 || offset >= len(obj.data) {
		return false
	}
	data := append([]byte(nil), obj.data...)
	data[offset] ^= 0x01
	obj.data = data
	m.objects[key] = obj
	return true
}

func (o memObject) info(key string) ObjectInfo {
	return ObjectInfo{Key: key, Size: int64(len(o.data)), LastModified: o.modified, Metadata: lowerKeys(o.metadata)}
}

func lowerKeys(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}
