package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	runContract(t, NewMem(), "contract/")
}

func TestMemStoreFailureInjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMem()
	boom := errors.New("boom")

	m.FailPut = boom
	if _, err := m.Put(ctx, "k", strings.NewReader("x"), PutOptions{}); !errors.Is(err, boom) {
		t.Errorf("FailPut: %v", err)
	}
	m.FailPut = nil

	m.FailPutAfterBytes = 100
	_, err := m.Put(ctx, "k", bytes.NewReader(make([]byte, 1000)), PutOptions{})
	if err == nil || !strings.Contains(err.Error(), "after 100 bytes") {
		t.Errorf("FailPutAfterBytes: %v", err)
	}
	if len(m.Keys()) != 0 {
		t.Error("failed Put stored an object")
	}
	if _, err := m.Put(ctx, "small", bytes.NewReader(make([]byte, 50)), PutOptions{}); err != nil {
		t.Errorf("Put below the limit failed: %v", err)
	}
	m.FailPutAfterBytes = 0

	m.FailStat = boom
	if _, err := m.Stat(ctx, "small"); !errors.Is(err, boom) {
		t.Errorf("FailStat: %v", err)
	}
	m.FailStat = nil
	m.FailGet = boom
	if _, _, err := m.Get(ctx, "small"); !errors.Is(err, boom) {
		t.Errorf("FailGet: %v", err)
	}
	m.FailGet = nil
	m.FailList = boom
	if _, err := m.List(ctx, ""); !errors.Is(err, boom) {
		t.Errorf("FailList: %v", err)
	}
	m.FailList = nil
	m.FailDelete = boom
	if err := m.Delete(ctx, []string{"small"}); !errors.Is(err, boom) {
		t.Errorf("FailDelete: %v", err)
	}
	m.FailDelete = nil

	if m.Puts != 3 || m.Stats != 1 || m.Gets != 1 || m.Lists != 1 || m.Deletes != 1 {
		t.Errorf("counters: puts=%d stats=%d gets=%d lists=%d deletes=%d", m.Puts, m.Stats, m.Gets, m.Lists, m.Deletes)
	}
}

func TestMemStoreHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMem()
	if _, err := m.Put(ctx, "obj", strings.NewReader("abcdef"), PutOptions{Metadata: map[string]string{"X-Upper": "v"}}); err != nil {
		t.Fatal(err)
	}
	if got := string(m.Bytes("obj")); got != "abcdef" {
		t.Errorf("Bytes = %q", got)
	}
	if m.Bytes("nope") != nil {
		t.Error("Bytes(missing) != nil")
	}
	if !m.Corrupt("obj", 2) || m.Corrupt("obj", 99) || m.Corrupt("nope", 0) {
		t.Error("Corrupt bounds")
	}
	rc, _, _ := m.Get(ctx, "obj")
	got, _ := io.ReadAll(rc)
	if string(got) != "abbdef" {
		t.Errorf("after Corrupt = %q", got)
	}
	info, _ := m.Stat(ctx, "obj")
	if info.Metadata["x-upper"] != "v" {
		t.Errorf("metadata keys not lowercased: %v", info.Metadata)
	}
	// Get returns a copy: mutating it does not change the store.
	got[0] = 'Z'
	if m.Bytes("obj")[0] == 'Z' {
		t.Error("Get aliased the stored bytes")
	}
}

func TestCountingReader(t *testing.T) {
	t.Parallel()
	cr := &countingReader{r: strings.NewReader("12345")}
	io.ReadAll(cr)
	if cr.n != 5 {
		t.Errorf("counted %d", cr.n)
	}
}

func TestNewS3Validation(t *testing.T) {
	t.Parallel()
	base := S3Config{Endpoint: "http://h:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"}
	if _, err := NewS3(base); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mod := range map[string]func(*S3Config){
		"no endpoint":   func(c *S3Config) { c.Endpoint = "" },
		"no bucket":     func(c *S3Config) { c.Bucket = "" },
		"no access key": func(c *S3Config) { c.AccessKey = "" },
		"no secret key": func(c *S3Config) { c.SecretKey = "" },
		"tiny part":     func(c *S3Config) { c.PartSize = 1024 },
	} {
		c := base
		mod(&c)
		if _, err := NewS3(c); err == nil {
			t.Errorf("%s: NewS3 succeeded", name)
		}
	}
	s, _ := NewS3(S3Config{Endpoint: "https://h", Bucket: "b", AccessKey: "a", SecretKey: "s", TLSInsecure: true})
	if s.partSize != defaultPartSize {
		t.Errorf("default part size = %d", s.partSize)
	}
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()
	if isNotFound(errors.New("random")) {
		t.Error("random error treated as not found")
	}
	if isNotFound(nil) {
		t.Error("nil treated as not found")
	}
}
