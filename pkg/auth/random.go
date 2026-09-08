package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// This lived alongside the JWT helpers until those were replaced by pkg/token.
// It is here rather than there because signing is one concern and generating an
// identifier is another, and the old file coupled both to a global config.

// RandomHex returns n random bytes as a hex string, so the result is 2n
// characters long.
//
// crypto/rand, never math/rand. Everything this produces is either an
// identifier that has to be unique across processes or a value that has to be
// unguessable, and a predictable generator fails both.
func RandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not something to paper over with a fallback:
		// a predictable value here is worse than no value at all.
		panic("auth: secure random unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
