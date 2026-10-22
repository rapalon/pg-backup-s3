package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// runContract exercises the ObjectStore behaviour every implementation must
// share. It is run against MemStore here and against Garage in the
// integration tests. prefix must be unique per run.
func runContract(t *testing.T, s ObjectStore, prefix string) {
	t.Helper()
	ctx := context.Background()

	k1 := prefix + "a/one.bin"
	k2 := prefix + "a/two.bin"
	k3 := prefix + "b/three.bin"
	payload := []byte("hello, object store")

	// Missing keys.
	if _, err := s.Stat(ctx, k1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat(missing) = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(ctx, k1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	if infos, err := s.List(ctx, prefix); err != nil || len(infos) != 0 {
		t.Fatalf("List(empty prefix) = %v, %v", infos, err)
	}

	// Put with metadata and content type.
	before := time.Now().Add(-2 * time.Second)
	res, err := s.Put(ctx, k1, bytes.NewReader(payload), PutOptions{
		ContentType: "application/octet-stream",
		Metadata:    map[string]string{"Sha256": "abc", "pgbackup-version": "0.1.0"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Size != int64(len(payload)) {
		t.Errorf("Put size = %d, want %d", res.Size, len(payload))
	}

	// Stat.
	info, err := s.Stat(ctx, k1)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != k1 || info.Size != int64(len(payload)) {
		t.Errorf("Stat = %+v", info)
	}
	if info.LastModified.Before(before) {
		t.Errorf("LastModified %v is before the upload", info.LastModified)
	}
	if info.Metadata["sha256"] != "abc" || info.Metadata["pgbackup-version"] != "0.1.0" {
		t.Logf("metadata not returned by Stat (store may not support x-amz-meta): %v", info.Metadata)
	}

	// Get.
	rc, ginfo, err := s.Get(ctx, k1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("Get content = %q (%v), want %q", got, err, payload)
	}
	if ginfo.Size != int64(len(payload)) {
		t.Errorf("Get info size = %d", ginfo.Size)
	}

	// Overwrite replaces.
	if _, err := s.Put(ctx, k1, bytes.NewReader([]byte("v2")), PutOptions{}); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	if info, _ := s.Stat(ctx, k1); info.Size != 2 {
		t.Errorf("overwrite size = %d, want 2", info.Size)
	}

	// List is recursive, prefix-scoped and sorted.
	for _, k := range []string{k3, k2} {
		if _, err := s.Put(ctx, k, bytes.NewReader(payload), PutOptions{}); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	infos, err := s.List(ctx, prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 3 || infos[0].Key != k1 || infos[1].Key != k2 || infos[2].Key != k3 {
		t.Fatalf("List = %v", keysOf(infos))
	}
	infos, _ = s.List(ctx, prefix+"a/")
	if len(infos) != 2 {
		t.Errorf("List(a/) = %v", keysOf(infos))
	}
	infos, _ = s.List(ctx, prefix+"a/t")
	if len(infos) != 1 || infos[0].Key != k2 {
		t.Errorf("List(a/t) = %v", keysOf(infos))
	}

	// Delete, including a missing key, then the prefix is empty.
	if err := s.Delete(ctx, []string{k1, k2, k3, prefix + "never-existed"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if infos, _ := s.List(ctx, prefix); len(infos) != 0 {
		t.Errorf("after Delete: %v", keysOf(infos))
	}
	if err := s.Delete(ctx, nil); err != nil {
		t.Errorf("Delete(nil) = %v", err)
	}

	// A reader that fails leaves nothing behind.
	bad := prefix + "bad.bin"
	_, err = s.Put(ctx, bad, &failingReader{limit: 1000, err: errors.New("source exploded")}, PutOptions{})
	if err == nil {
		t.Fatal("Put with a failing reader succeeded")
	}
	if _, err := s.Stat(ctx, bad); !errors.Is(err, ErrNotFound) {
		t.Errorf("object exists after failed Put: %v", err)
	}

	// Cancelled context fails.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Put(cctx, prefix+"cancelled.bin", bytes.NewReader(payload), PutOptions{}); err == nil {
		t.Error("Put with cancelled context succeeded")
	}
}

func keysOf(infos []ObjectInfo) []string {
	out := make([]string, len(infos))
	for i, o := range infos {
		out[i] = o.Key
	}
	return out
}

// failingReader yields deterministic bytes, then fails after limit bytes.
type failingReader struct {
	limit int64
	read  int64
	err   error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.read >= f.limit {
		return 0, f.err
	}
	n := len(p)
	if rem := f.limit - f.read; int64(n) > rem {
		n = int(rem)
	}
	for i := range p[:n] {
		p[i] = byte(f.read + int64(i))
	}
	f.read += int64(n)
	return n, nil
}
