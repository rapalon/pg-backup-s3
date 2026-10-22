// Package crypt wraps filippo.io/age as streaming writers and readers.
//
// Backups are encrypted to one or more X25519 recipients (public keys, the
// only key material present in the backup container) or, alternatively, to a
// passphrase (scrypt). Decryption needs the matching X25519 identity or the
// passphrase. The format is standard age: any .age file produced here opens
// with the age or rage CLI, so recovery never depends on this binary.
package crypt

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

var (
	// ErrNoRecipients is returned when no recipient could be built.
	ErrNoRecipients = errors.New("no age recipients configured")
	// ErrNoIdentities is returned when no identity could be built.
	ErrNoIdentities = errors.New("no age identities configured")
)

// scryptWorkFactor is age's default (2^18 iterations, ~1 s). Tests lower it.
var scryptWorkFactor = 18

// ParseRecipients parses comma- or newline-separated X25519 public keys
// ("age1..."). Blank lines and lines starting with '#' are ignored.
func ParseRecipients(s string) ([]age.Recipient, error) {
	var out []age.Recipient
	for _, tok := range tokens(s) {
		r, err := age.ParseX25519Recipient(tok)
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", tok, err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, ErrNoRecipients
	}
	return out, nil
}

// ParseIdentities parses comma- or newline-separated X25519 private keys
// ("AGE-SECRET-KEY-1..."). Blank lines and '#' comments are ignored, so the
// contents of an `age-keygen` output file can be passed as is.
func ParseIdentities(s string) ([]age.Identity, error) {
	var out []age.Identity
	for _, tok := range tokens(s) {
		id, err := age.ParseX25519Identity(tok)
		if err != nil {
			// Never echo a would-be secret back in the error.
			return nil, fmt.Errorf("identity #%d: %w", len(out)+1, err)
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, ErrNoIdentities
	}
	return out, nil
}

// PassphraseRecipient builds an scrypt recipient. age refuses to combine it
// with other recipients, so passphrase mode is always exclusive.
func PassphraseRecipient(passphrase string) (age.Recipient, error) {
	if passphrase == "" {
		return nil, errors.New("empty passphrase")
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	r.SetWorkFactor(scryptWorkFactor)
	return r, nil
}

// PassphraseIdentity builds the identity that decrypts passphrase-encrypted data.
func PassphraseIdentity(passphrase string) (age.Identity, error) {
	if passphrase == "" {
		return nil, errors.New("empty passphrase")
	}
	return age.NewScryptIdentity(passphrase)
}

// RecipientsFor builds the recipient list from the configuration values:
// a passphrase if set, otherwise the X25519 recipients.
func RecipientsFor(recipients, passphrase string) ([]age.Recipient, error) {
	if passphrase != "" {
		r, err := PassphraseRecipient(passphrase)
		if err != nil {
			return nil, err
		}
		return []age.Recipient{r}, nil
	}
	if strings.TrimSpace(recipients) == "" {
		return nil, ErrNoRecipients
	}
	return ParseRecipients(recipients)
}

// IdentitiesFor builds every identity available for decryption from the
// configuration values: the X25519 identities and/or the passphrase.
func IdentitiesFor(identities, passphrase string) ([]age.Identity, error) {
	var out []age.Identity
	if strings.TrimSpace(identities) != "" {
		ids, err := ParseIdentities(identities)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	if passphrase != "" {
		id, err := PassphraseIdentity(passphrase)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, ErrNoIdentities
	}
	return out, nil
}

// NewEncryptor returns a writer that encrypts everything written to it and
// streams the ciphertext to w. Close must be called to flush the final chunk;
// without it the output is truncated and will not decrypt.
func NewEncryptor(w io.Writer, recipients []age.Recipient) (io.WriteCloser, error) {
	if len(recipients) == 0 {
		return nil, ErrNoRecipients
	}
	return age.Encrypt(w, recipients...)
}

// NewDecryptor returns a reader that decrypts the age stream in r. The header
// is read and the identities tried immediately, so a wrong key fails here
// (with *age.NoIdentityMatchError). Each 64 KiB chunk is authenticated before
// any of its plaintext is returned; corruption or truncation surfaces as a
// read error, never as wrong bytes.
func NewDecryptor(r io.Reader, identities []age.Identity) (io.Reader, error) {
	if len(identities) == 0 {
		return nil, ErrNoIdentities
	}
	return age.Decrypt(r, identities...)
}

// tokens splits a key list on commas and newlines, dropping blanks and comments.
func tokens(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		tok = strings.TrimSpace(tok)
		if tok == "" || strings.HasPrefix(tok, "#") {
			continue
		}
		out = append(out, tok)
	}
	return out
}
