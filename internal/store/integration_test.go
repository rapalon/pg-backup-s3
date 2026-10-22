//go:build integration

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func standStore(t *testing.T) *S3Store {
	t.Helper()
	if os.Getenv("PGBACKUP_INTEGRATION") != "1" {
		t.Skip("PGBACKUP_INTEGRATION != 1; run via test/compose.yml")
	}
	s, err := NewS3(S3Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		Region:    os.Getenv("S3_REGION"),
		Bucket:    os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_SECRET_KEY"),
		PathStyle: os.Getenv("S3_PATH_STYLE") != "false",
		PartSize:  8 << 20, // small parts so the 100 MiB test is real multipart
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s
}

func uniquePrefix(t *testing.T) string {
	return fmt.Sprintf("it/%s/%d/", strings.ToLower(t.Name()), time.Now().UnixNano())
}

func cleanup(t *testing.T, s *S3Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	infos, err := s.List(ctx, prefix)
	if err != nil {
		t.Logf("cleanup list: %v", err)
		return
	}
	if err := s.Delete(ctx, keysOf(infos)); err != nil {
		t.Logf("cleanup delete: %v", err)
	}
}

func TestIntegrationPing(t *testing.T) {
	s := standStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	wrong, _ := NewS3(S3Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		Region:    os.Getenv("S3_REGION"),
		Bucket:    "does-not-exist-" + fmt.Sprint(time.Now().UnixNano()),
		AccessKey: os.Getenv("S3_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_SECRET_KEY"),
		PathStyle: true,
	})
	err := wrong.Ping(context.Background())
	if !errors.Is(err, ErrBucketNotFound) {
		t.Errorf("Ping(missing bucket) = %v, want ErrBucketNotFound", err)
	}
}

func TestIntegrationContract(t *testing.T) {
	s := standStore(t)
	prefix := uniquePrefix(t)
	defer cleanup(t, s, prefix)
	runContract(t, s, prefix)
}

func TestIntegrationLargeMultipartRoundTrip(t *testing.T) {
	s := standStore(t)
	prefix := uniquePrefix(t)
	defer cleanup(t, s, prefix)
	ctx := context.Background()
	const size = 100 << 20
	key := prefix + "large.bin"

	// Hash the stream as it is uploaded so nothing is held in memory.
	src := newPatternReader(42, size)
	pr, pw := io.Pipe()
	hashCh := make(chan string, 1)
	go func() {
		h, _ := hashOf(t, io.TeeReader(src, pw))
		pw.Close()
		hashCh <- h
	}()

	start := time.Now()
	res, err := s.Put(ctx, key, pr, PutOptions{Metadata: map[string]string{"sha256": "pending"}})
	if err != nil {
		t.Fatalf("Put 100 MiB: %v", err)
	}
	wantHash := <-hashCh
	t.Logf("uploaded %d MiB in %s (etag %s)", res.Size>>20, time.Since(start).Round(time.Millisecond), res.ETag)
	if res.Size != size {
		t.Errorf("Put size = %d, want %d", res.Size, size)
	}

	info, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != size {
		t.Errorf("Stat size = %d, want %d", info.Size, size)
	}
	if info.Metadata["sha256"] != "pending" {
		t.Logf("user metadata not returned by this server: %v", info.Metadata)
	}

	rc, ginfo, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	gotHash, n := hashOf(t, rc)
	rc.Close()
	if n != size || ginfo.Size != size {
		t.Errorf("Get returned %d bytes (info %d), want %d", n, ginfo.Size, size)
	}
	if gotHash != wantHash {
		t.Error("downloaded content differs from uploaded content")
	}

	if uploads, err := s.IncompleteUploads(ctx, prefix); err != nil || len(uploads) != 0 {
		t.Errorf("incomplete uploads after success: %v %v", uploads, err)
	}
}

func TestIntegrationFailedMultipartLeavesNothing(t *testing.T) {
	s := standStore(t)
	prefix := uniquePrefix(t)
	defer cleanup(t, s, prefix)
	ctx := context.Background()
	key := prefix + "broken.bin"

	_, err := s.Put(ctx, key, &failingReader{limit: 40 << 20, err: errors.New("pg_dump died")}, PutOptions{})
	if err == nil {
		t.Fatal("Put with a reader failing at 40 MiB succeeded")
	}
	if !strings.Contains(err.Error(), "pg_dump died") {
		t.Errorf("error does not carry the reader's error: %v", err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("object exists after failed upload: %v", err)
	}
	uploads, err := s.IncompleteUploads(ctx, prefix)
	if err != nil {
		t.Fatalf("IncompleteUploads: %v", err)
	}
	if len(uploads) != 0 {
		t.Errorf("multipart upload was not aborted: %v", uploads)
	}
}

func TestIntegrationCancelledMultipartLeavesNothing(t *testing.T) {
	s := standStore(t)
	prefix := uniquePrefix(t)
	defer cleanup(t, s, prefix)
	key := prefix + "cancelled.bin"

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once ~20 MiB have been read from the source.
	src := newPatternReader(7, 200<<20)
	var read int64
	tripwire := readerFunc(func(p []byte) (int, error) {
		n, err := src.Read(p)
		read += int64(n)
		if read > 20<<20 {
			cancel()
		}
		return n, err
	})
	_, err := s.Put(ctx, key, tripwire, PutOptions{})
	cancel()
	if err == nil {
		t.Fatal("Put survived context cancellation")
	}
	bg := context.Background()
	if _, err := s.Stat(bg, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("object exists after cancelled upload: %v", err)
	}
	if uploads, err := s.IncompleteUploads(bg, prefix); err != nil || len(uploads) != 0 {
		t.Errorf("incomplete uploads after cancel: %v %v", uploads, err)
	}
}

func TestIntegrationMetadataRoundTrip(t *testing.T) {
	s := standStore(t)
	prefix := uniquePrefix(t)
	defer cleanup(t, s, prefix)
	ctx := context.Background()
	key := prefix + "meta.bin"
	meta := map[string]string{"sha256": "0123abcd", "pgbackup-version": "0.1.0"}
	if _, err := s.Put(ctx, key, bytes.NewReader([]byte("x")), PutOptions{ContentType: "application/octet-stream", Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range meta {
		if info.Metadata[k] != v {
			t.Skipf("server does not return user metadata (%s=%q, got %v); manifests remain the source of truth", k, v, info.Metadata)
		}
	}
	_, ginfo, err := s.Get(ctx, key)
	if err != nil || ginfo.Metadata["sha256"] != "0123abcd" {
		t.Errorf("Get metadata = %v (%v)", ginfo.Metadata, err)
	}
}

func TestIntegrationDeleteManyAndPagination(t *testing.T) {
	s := standStore(t)
	prefix := uniquePrefix(t)
	defer cleanup(t, s, prefix)
	ctx := context.Background()

	const n = 1100 // more than one DeleteObjects batch and one List page
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%sobj-%05d", prefix, i)
		if _, err := s.Put(ctx, keys[i], bytes.NewReader([]byte{byte(i)}), PutOptions{}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	infos, err := s.List(ctx, prefix)
	if err != nil || len(infos) != n {
		t.Fatalf("List = %d objects (%v), want %d", len(infos), err, n)
	}
	for i := 1; i < len(infos); i++ {
		if infos[i-1].Key >= infos[i].Key {
			t.Fatalf("List not sorted at %d", i)
		}
	}
	if err := s.Delete(ctx, keys); err != nil {
		t.Fatalf("Delete %d: %v", n, err)
	}
	if infos, _ := s.List(ctx, prefix); len(infos) != 0 {
		t.Errorf("%d objects left after Delete", len(infos))
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
