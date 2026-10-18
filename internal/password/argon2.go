// Package password hache et vérifie les mots de passe (Argon2id, format PHC).
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Paramètres recommandés par la RFC 9106 (second choix, mémoire réduite).
const (
	memory  = 64 * 1024
	time_   = 3
	threads = 2
	keyLen  = 32
	saltLen = 16
)

// MinLength est la longueur minimale d'un mot de passe, en caractères.
const MinLength = 12

// ErrTooShort : mot de passe trop court.
var ErrTooShort = fmt.Errorf("le mot de passe doit faire au moins %d caractères", MinLength)

// Check refuse les mots de passe trop faibles.
func Check(pw string) error {
	if utf8.RuneCountInString(pw) < MinLength {
		return ErrTooShort
	}
	return nil
}

// Hash renvoie $argon2id$v=19$m=…,t=…,p=…$sel$hachage.
func Hash(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, time_, memory, threads, keyLen)
	b := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, memory, time_, threads,
		b.EncodeToString(salt), b.EncodeToString(key)), nil
}

// ErrFormat : hachage illisible.
var ErrFormat = errors.New("hachage Argon2id illisible")

// Verify compare un mot de passe à un hachage, en temps constant.
func Verify(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrFormat
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, ErrFormat
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrFormat
	}
	b := base64.RawStdEncoding
	salt, err := b.DecodeString(parts[4])
	if err != nil {
		return false, ErrFormat
	}
	want, err := b.DecodeString(parts[5])
	if err != nil {
		return false, ErrFormat
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Dummy est un hachage valide utilisé quand le compte n'existe pas, pour que
// la réponse prenne le même temps.
var Dummy, _ = Hash("compte-inexistant-lattis")
