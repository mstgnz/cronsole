package token

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "a-test-secret-that-is-long-enough-to-sign"

func TestIssueAndVerify(t *testing.T) {
	issuer := NewIssuer(testSecret)

	signed, err := issuer.Issue(42)
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}

	userID, issuedAt, err := issuer.Verify(signed)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if userID != 42 {
		t.Errorf("user id = %d, want 42", userID)
	}
	if time.Since(issuedAt) > time.Minute {
		t.Errorf("issued at = %s, which is not recent", issuedAt)
	}
}

func TestVerifyRejectsAnotherKey(t *testing.T) {
	signed, err := NewIssuer(testSecret).Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewIssuer("a-different-secret-of-sufficient-length").Verify(signed); !errors.Is(err, ErrInvalid) {
		t.Errorf("a token signed with another key gave %v, want ErrInvalid", err)
	}
}

func TestVerifyRejectsAlgNone(t *testing.T) {
	// Trusting the header's algorithm is how a token signed with "none" gets
	// accepted. The algorithm is pinned instead.
	claims := jwt.RegisteredClaims{
		Subject:   "1",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewIssuer(testSecret).Verify(unsigned); !errors.Is(err, ErrInvalid) {
		t.Errorf(`a token with alg "none" gave %v, want ErrInvalid`, err)
	}
}

func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	claims := jwt.RegisteredClaims{
		Subject:   "1",
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-48 * time.Hour)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-24 * time.Hour)),
	}
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewIssuer(testSecret).Verify(expired); !errors.Is(err, ErrInvalid) {
		t.Errorf("an expired token gave %v, want ErrInvalid", err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	issuer := NewIssuer(testSecret)
	for _, raw := range []string{"", "not.a.token", "a.b", strings.Repeat("x", 500)} {
		if _, _, err := issuer.Verify(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("Verify(%q) gave %v, want ErrInvalid", raw, err)
		}
	}
}

func TestVerifyRequiresAnIssueTime(t *testing.T) {
	// The issue time is what makes logout and a password change able to retire
	// a token that is otherwise still inside its expiry, so a token without
	// one cannot be honoured.
	claims := jwt.RegisteredClaims{
		Subject:   "1",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	noIat, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewIssuer(testSecret).Verify(noIat); !errors.Is(err, ErrInvalid) {
		t.Errorf("a token with no issue time gave %v, want ErrInvalid", err)
	}
}

func TestVerifyRejectsANonNumericSubject(t *testing.T) {
	claims := jwt.RegisteredClaims{
		Subject:   "not-a-number",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	odd, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewIssuer(testSecret).Verify(odd); !errors.Is(err, ErrInvalid) {
		t.Errorf("a non numeric subject gave %v, want ErrInvalid", err)
	}
}
