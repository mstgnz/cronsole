package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashAndSaltRoundTrips(t *testing.T) {
	hash := HashAndSalt("correct horse battery staple")

	if !ComparePassword(hash, "correct horse battery staple") {
		t.Error("the password did not verify against its own hash")
	}
	if ComparePassword(hash, "correct horse battery stapl") {
		t.Error("a wrong password verified")
	}
	if ComparePassword(hash, "") {
		t.Error("an empty password verified")
	}
}

func TestTheSamePasswordHashesDifferentlyEveryTime(t *testing.T) {
	// bcrypt salts each hash. Two identical hashes in the users table would say
	// two accounts share a password, which is a fact nobody should be able to
	// read out of a stolen dump.
	a := HashAndSalt("same password")
	b := HashAndSalt("same password")
	if a == b {
		t.Error("two hashes of one password are identical, so the salt is not being used")
	}
}

func TestTheCostIsAboveTheLibraryDefault(t *testing.T) {
	// bcrypt.DefaultCost is 10, which is below what OWASP ASVS asks for. The
	// cost is embedded in the hash, so this is checked from the hash rather
	// than from the constant: that is what a stolen dump would actually have.
	cost, err := bcrypt.Cost([]byte(HashAndSalt("x")))
	if err != nil {
		t.Fatalf("the hash is not a bcrypt hash: %v", err)
	}
	if cost < 12 {
		t.Errorf("cost = %d, want at least 12", cost)
	}
}

func TestComparePasswordRefusesRubbish(t *testing.T) {
	// A hash column that was truncated, empty or hand-edited must fail closed.
	for _, hash := range []string{"", "not-a-hash", "$2a$12$tooshort", strings.Repeat("x", 60)} {
		if ComparePassword(hash, "anything") {
			t.Errorf("a malformed hash %q verified", hash)
		}
	}
}

func TestDummyHashIsUsableForTheTimingEqualiser(t *testing.T) {
	// It is compared against when the address is unknown, so a login attempt
	// for a non-existent account costs the same as one for a real account. That
	// only holds if it is a real hash at the real cost, and if nothing verifies
	// against it.
	cost, err := bcrypt.Cost([]byte(DummyHash))
	if err != nil {
		t.Fatalf("DummyHash is not a bcrypt hash: %v", err)
	}
	if cost < 12 {
		t.Errorf("DummyHash cost = %d, want at least 12, or the timing differs", cost)
	}
	for _, guess := range []string{"", "password", "admin", "dummy"} {
		if ComparePassword(DummyHash, guess) {
			t.Fatalf("DummyHash verified against %q, so it is a working credential", guess)
		}
	}
}

func TestRandomHexLengthAndAlphabet(t *testing.T) {
	// n bytes render as 2n hex characters. Everything built on this is either
	// an identifier that must be unique across processes or a value that must
	// be unguessable.
	for _, n := range []int{1, 8, 16, 32} {
		got := RandomHex(n)
		if len(got) != n*2 {
			t.Errorf("RandomHex(%d) is %d characters, want %d", n, len(got), n*2)
		}
		if strings.Trim(got, "0123456789abcdef") != "" {
			t.Errorf("RandomHex(%d) = %q, which is not hex", n, got)
		}
	}
}

func TestRandomHexDoesNotRepeat(t *testing.T) {
	// Not a statistical test of the generator, which is crypto/rand's job. This
	// catches the failure that has actually happened in the wild: a seed fixed
	// at start up, so every value in a process is the same.
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		v := RandomHex(16)
		if seen[v] {
			t.Fatalf("RandomHex repeated %q within 1000 draws", v)
		}
		seen[v] = true
	}
}

func TestRandomHexOfNothing(t *testing.T) {
	if got := RandomHex(0); got != "" {
		t.Errorf("RandomHex(0) = %q, want the empty string", got)
	}
}
