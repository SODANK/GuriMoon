package db

// Mailed keys: how a pilot proves an in-game-browser identity. The IGB identity headers are
// forgeable, so the server mails a key to the pilot named in the header and the page asks for
// it back. The Intergalactic Summit uses these; so does the SCC module's brokerage.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	mailedKeyAlphabet  = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1: read off a mail by eye
	mailedKeyMaxFails  = 5
	mailedKeyLockSecs  = 15 * 60
	mailedKeyGroupSize = 4
	mailedKeyGroups    = 3
)

// newMailedKey draws <prefix>-XXXX-XXXX-XXXX: the shape of every key the server mails a pilot to
// prove an in-game-browser identity (SCC brokerage, Intergalactic Summit).
func newMailedKey(prefix string) (string, error) {
	buf := make([]byte, mailedKeyGroups*mailedKeyGroupSize)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(prefix)
	for i, c := range buf {
		if i%mailedKeyGroupSize == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(mailedKeyAlphabet[int(c)%len(mailedKeyAlphabet)])
	}
	return b.String(), nil
}

// normalizeMailedKey forgives case, spaces, dashes and a missing prefix.
func normalizeMailedKey(k, prefix string) string {
	k = strings.ToUpper(strings.Join(strings.Fields(k), ""))
	k = strings.ReplaceAll(k, "-", "")
	k = strings.TrimPrefix(k, prefix)
	return k
}

// hashMailedKey is the stored form of a normalized key; scope keeps one service's hashes from
// matching another's.
func hashMailedKey(scope, normalized string) string {
	sum := sha256.Sum256([]byte(scope + normalized))
	return hex.EncodeToString(sum[:])
}
