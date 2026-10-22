package crypt

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
)

func init() {
	scryptWorkFactor = 10 // keep passphrase tests fast
}

func newKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func encrypt(t *testing.T, plaintext []byte, recipients ...age.Recipient) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewEncryptor(&buf, recipients)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func decrypt(t *testing.T, ciphertext []byte, identities ...age.Identity) ([]byte, error) {
	t.Helper()
	r, err := NewDecryptor(bytes.NewReader(ciphertext), identities)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// testData returns deterministic pseudo-random bytes spanning several chunks.
func testData(n int) []byte {
	rng := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func TestRoundTripOneRecipient(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	plain := testData(200 << 10)
	ct := encrypt(t, plain, key.Recipient())
	if bytes.Contains(ct, plain[:64]) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := decrypt(t, ct, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("round trip mismatch")
	}
}

func TestRoundTripThreeRecipientsSecondKeyOnly(t *testing.T) {
	t.Parallel()
	k1, k2, k3 := newKey(t), newKey(t), newKey(t)
	plain := testData(100 << 10)
	ct := encrypt(t, plain, k1.Recipient(), k2.Recipient(), k3.Recipient())

	got, err := decrypt(t, ct, k2)
	if err != nil {
		t.Fatalf("decrypt with second key: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("mismatch with second key")
	}

	stranger := newKey(t)
	_, err = decrypt(t, ct, stranger)
	var noMatch *age.NoIdentityMatchError
	if !errors.As(err, &noMatch) {
		t.Errorf("stranger: err = %v, want *age.NoIdentityMatchError", err)
	}
}

func TestPassphraseRoundTrip(t *testing.T) {
	t.Parallel()
	recs, err := RecipientsFor("", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	plain := testData(70 << 10)
	ct := encrypt(t, plain, recs...)

	ids, err := IdentitiesFor("", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	got, err := decrypt(t, ct, ids...)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("passphrase round trip: err=%v match=%v", err, bytes.Equal(got, plain))
	}

	wrong, _ := IdentitiesFor("", "wrong horse")
	if _, err := decrypt(t, ct, wrong...); err == nil {
		t.Error("wrong passphrase decrypted")
	}
}

func TestEmptyPlaintext(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	ct := encrypt(t, nil, key.Recipient())
	got, err := decrypt(t, ct, key)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: got %d bytes, err %v", len(got), err)
	}
}

func TestFlippedByteIsDetectedBeforeOutput(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	plain := testData(200 << 10) // 4 chunks of 64 KiB
	ct := encrypt(t, plain, key.Recipient())

	for name, pos := range map[string]int{
		"header":      10,
		"first chunk": 400,
		"last chunk":  len(ct) - 100,
	} {
		tampered := append([]byte(nil), ct...)
		tampered[pos] ^= 0x01

		r, err := NewDecryptor(bytes.NewReader(tampered), []age.Identity{key})
		if err != nil {
			if name != "header" {
				t.Errorf("%s: NewDecryptor failed early: %v", name, err)
			}
			continue
		}
		got, err := io.ReadAll(r)
		if err == nil {
			t.Errorf("%s: corrupted stream read without error", name)
		}
		// Whatever was returned before the error must be a true prefix.
		if !bytes.Equal(got, plain[:len(got)]) {
			t.Errorf("%s: corrupted plaintext was returned", name)
		}
		if len(got) >= len(plain) {
			t.Errorf("%s: full plaintext returned despite corruption", name)
		}
	}
}

func TestTruncationIsDetected(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	plain := testData(150 << 10)
	ct := encrypt(t, plain, key.Recipient())

	for name, cut := range map[string]int{
		"last 50 bytes":  50,
		"half":           len(ct) / 2,
		"whole last tag": 16,
	} {
		truncated := ct[:len(ct)-cut]
		r, err := NewDecryptor(bytes.NewReader(truncated), []age.Identity{key})
		if err != nil {
			t.Fatalf("%s: NewDecryptor: %v", name, err)
		}
		got, err := io.ReadAll(r)
		if err == nil {
			t.Errorf("%s: truncated stream ended with clean EOF", name)
		}
		if !bytes.Equal(got, plain[:len(got)]) {
			t.Errorf("%s: corrupted plaintext returned", name)
		}
	}
}

func TestEncryptorWithoutCloseIsIncomplete(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	var buf bytes.Buffer
	w, _ := NewEncryptor(&buf, []age.Recipient{key.Recipient()})
	w.Write(testData(10 << 10))
	// no Close
	r, err := NewDecryptor(bytes.NewReader(buf.Bytes()), []age.Identity{key})
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Error("stream without final chunk decrypted cleanly")
	}
}

func TestStreamingMemoryIsBounded(t *testing.T) {
	size := 64 << 20
	if testing.Short() {
		size = 8 << 20
	}
	key := newKey(t)

	// Sample heap usage while the pipeline runs; a buffering implementation
	// would hold the whole 64 MiB.
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	pr, pw := io.Pipe()
	inHash := sha256.New()
	go func() {
		enc, err := NewEncryptor(pw, []age.Recipient{key.Recipient()})
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		chunk := testData(1 << 20)
		for written := 0; written < size; written += len(chunk) {
			inHash.Write(chunk)
			if _, err := enc.Write(chunk); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.CloseWithError(enc.Close())
	}()

	dec, err := NewDecryptor(pr, []age.Identity{key})
	if err != nil {
		t.Fatalf("NewDecryptor: %v", err)
	}
	outHash := sha256.New()
	n, err := io.Copy(outHash, dec)
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if int(n) != size {
		t.Fatalf("decrypted %d bytes, want %d", n, size)
	}
	if !bytes.Equal(inHash.Sum(nil), outHash.Sum(nil)) {
		t.Fatal("hash mismatch after streaming")
	}
	if ceiling := uint64(size) / 2; peak.Load() > ceiling {
		t.Errorf("peak heap %d MiB exceeds %d MiB: the stream is being buffered", peak.Load()>>20, ceiling>>20)
	}
	t.Logf("peak heap during %d MiB stream: %d MiB", size>>20, peak.Load()>>20)
}

func TestParseRecipients(t *testing.T) {
	t.Parallel()
	k1, k2 := newKey(t), newKey(t)
	input := "# ops key\n" + k1.Recipient().String() + "\r\n\n  " + k2.Recipient().String() + " ,\n"
	recs, err := ParseRecipients(input)
	if err != nil {
		t.Fatalf("ParseRecipients: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d recipients, want 2", len(recs))
	}
	comma := k1.Recipient().String() + "," + k2.Recipient().String()
	if recs, err := ParseRecipients(comma); err != nil || len(recs) != 2 {
		t.Errorf("comma separated: %d %v", len(recs), err)
	}

	for name, in := range map[string]string{
		"empty":           "",
		"only comments":   "# nothing\n",
		"identity":        k1.String(),
		"garbage":         "age1notakey",
		"one bad of two":  k1.Recipient().String() + ",nope",
		"passphrase word": "hunter2",
	} {
		if _, err := ParseRecipients(in); err == nil {
			t.Errorf("%s: ParseRecipients succeeded", name)
		}
	}
	if _, err := ParseRecipients(""); !errors.Is(err, ErrNoRecipients) {
		t.Errorf("empty: err = %v, want ErrNoRecipients", err)
	}
}

func TestParseIdentities(t *testing.T) {
	t.Parallel()
	k1, k2 := newKey(t), newKey(t)
	keygenFile := "# created: 2026-10-22\n# public key: " + k1.Recipient().String() + "\n" + k1.String() + "\n"
	ids, err := ParseIdentities(keygenFile)
	if err != nil || len(ids) != 1 {
		t.Fatalf("keygen file: %d %v", len(ids), err)
	}
	ids, err = ParseIdentities(k1.String() + "," + k2.String())
	if err != nil || len(ids) != 2 {
		t.Fatalf("two identities: %d %v", len(ids), err)
	}

	_, err = ParseIdentities(k1.Recipient().String())
	if err == nil {
		t.Error("recipient accepted as identity")
	}
	if strings.Contains(err.Error(), k1.Recipient().String()) {
		t.Error("error echoes the input")
	}
	if _, err := ParseIdentities(" \n"); !errors.Is(err, ErrNoIdentities) {
		t.Errorf("blank: err = %v, want ErrNoIdentities", err)
	}
}

func TestRecipientsForAndIdentitiesFor(t *testing.T) {
	t.Parallel()
	k := newKey(t)

	recs, err := RecipientsFor(k.Recipient().String(), "")
	if err != nil || len(recs) != 1 {
		t.Errorf("RecipientsFor(key): %d %v", len(recs), err)
	}
	if _, err := RecipientsFor("", ""); !errors.Is(err, ErrNoRecipients) {
		t.Errorf("RecipientsFor(empty): %v", err)
	}
	if _, err := RecipientsFor(" \n", ""); !errors.Is(err, ErrNoRecipients) {
		t.Errorf("RecipientsFor(blank): %v", err)
	}
	recs, err = RecipientsFor(k.Recipient().String(), "pw")
	if err != nil || len(recs) != 1 {
		t.Errorf("RecipientsFor(passphrase wins): %d %v", len(recs), err)
	}
	if _, ok := recs[0].(*age.ScryptRecipient); !ok {
		t.Errorf("passphrase should produce an scrypt recipient, got %T", recs[0])
	}

	ids, err := IdentitiesFor(k.String(), "pw")
	if err != nil || len(ids) != 2 {
		t.Errorf("IdentitiesFor(both): %d %v", len(ids), err)
	}
	if _, err := IdentitiesFor("", ""); !errors.Is(err, ErrNoIdentities) {
		t.Errorf("IdentitiesFor(empty): %v", err)
	}
	if _, err := IdentitiesFor("bad", ""); err == nil {
		t.Error("IdentitiesFor(bad) succeeded")
	}
}

func TestEmptyKeyMaterialErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewEncryptor(io.Discard, nil); !errors.Is(err, ErrNoRecipients) {
		t.Errorf("NewEncryptor(nil) = %v", err)
	}
	if _, err := NewDecryptor(strings.NewReader(""), nil); !errors.Is(err, ErrNoIdentities) {
		t.Errorf("NewDecryptor(nil) = %v", err)
	}
	if _, err := PassphraseRecipient(""); err == nil {
		t.Error("PassphraseRecipient(\"\") succeeded")
	}
	if _, err := PassphraseIdentity(""); err == nil {
		t.Error("PassphraseIdentity(\"\") succeeded")
	}
}

func TestNotAnAgeStream(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	_, err := NewDecryptor(strings.NewReader("PGDMP this is a raw pg_dump archive, not age"), []age.Identity{key})
	if err == nil {
		t.Error("plain data accepted as age stream")
	}
}
