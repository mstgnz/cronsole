// Package token issues and verifies session tokens.
//
// The signing key is held by the issuer, not read from a global at call time.
// A global secret is how a package ends up signing with an empty key because
// it ran before configuration was loaded, and the failure looks exactly like a
// working system.
package token

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TTL is how long an issued token stays valid. The auth cookie is given the
// same lifetime so the browser never holds a cookie that outlives its token.
const TTL = 24 * time.Hour

// ErrInvalid is returned for anything that is not a token this issuer signed
// and would accept right now. The reason is deliberately not distinguished:
// telling a caller whether a token was expired, forged or malformed helps only
// the caller who is guessing.
var ErrInvalid = errors.New("invalid token")

// Issuer signs and verifies tokens with one key.
type Issuer struct {
	secret []byte
}

// NewIssuer builds an issuer over a signing key.
func NewIssuer(secret string) *Issuer { return &Issuer{secret: []byte(secret)} }

// Issue mints a token for a user.
func (i *Issuer) Issue(userID int64) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(TTL)),
		Subject:   strconv.FormatInt(userID, 10),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
}

// Verify returns the user id and the moment the token was issued.
//
// The issue time is what makes logout and password changes able to retire a
// token that is otherwise still inside its expiry, so it is part of the
// contract rather than an extra.
func (i *Issuer) Verify(raw string) (int64, time.Time, error) {
	parsed, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		// The algorithm is pinned rather than taken from the header. Trusting
		// the header is how a token signed with "none", or an RSA public key
		// used as an HMAC secret, gets accepted.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return i.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return 0, time.Time{}, ErrInvalid
	}

	claims, ok := parsed.Claims.(jwt.RegisteredClaims)
	if !ok {
		mapped, ok := parsed.Claims.(jwt.MapClaims)
		if !ok {
			return 0, time.Time{}, ErrInvalid
		}
		return fromMap(mapped)
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || claims.IssuedAt == nil {
		return 0, time.Time{}, ErrInvalid
	}
	return id, claims.IssuedAt.Time, nil
}

func fromMap(claims jwt.MapClaims) (int64, time.Time, error) {
	subject, err := claims.GetSubject()
	if err != nil || subject == "" {
		return 0, time.Time{}, ErrInvalid
	}
	id, err := strconv.ParseInt(subject, 10, 64)
	if err != nil {
		return 0, time.Time{}, ErrInvalid
	}
	issuedAt, err := claims.GetIssuedAt()
	if err != nil || issuedAt == nil {
		return 0, time.Time{}, ErrInvalid
	}
	return id, issuedAt.Time, nil
}
