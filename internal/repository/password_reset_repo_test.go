package repository

import (
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// seedResetUser creates an account to hang reset links from.
func seedResetUser(t *testing.T, s *Store, email string) int64 {
	t.Helper()

	id, err := NewUserRepo(s).Create(ctx(t), &domain.User{
		Fullname: "Operator", Email: email, Password: "x", Active: true,
	})
	mustNoErr(t, err, "Create user")
	return id
}

func TestAResetLinkIsFoundByItsHash(t *testing.T) {
	s := fresh(t)
	repo := NewPasswordResetRepo(s)
	userID := seedResetUser(t, s, "operator@example.com")

	expires := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	id, err := repo.Create(ctx(t), &domain.PasswordReset{
		UserID: userID, TokenHash: "hash-of-the-mailed-token", ExpiresAt: expires,
	})
	mustNoErr(t, err, "Create")

	got, err := repo.FindByTokenHash(ctx(t), "hash-of-the-mailed-token")
	mustNoErr(t, err, "FindByTokenHash")
	if got.ID != id || got.UserID != userID {
		t.Errorf("found id=%d user=%d, want id=%d user=%d", got.ID, got.UserID, id, userID)
	}
	if got.UsedAt != nil {
		t.Error("a new link is already spent")
	}
	if !got.Live(time.Now()) {
		t.Error("a new link is not live")
	}

	if _, err := repo.FindByTokenHash(ctx(t), "a-hash-nobody-issued"); err == nil {
		t.Error("an unknown hash was found")
	}
}

func TestTheSameTokenCannotBeIssuedTwice(t *testing.T) {
	// The unique index is the guard. Two rows with one hash would mean one
	// mailed link that resolves to two accounts.
	s := fresh(t)
	repo := NewPasswordResetRepo(s)
	first := seedResetUser(t, s, "first@example.com")
	second := seedResetUser(t, s, "second@example.com")

	expires := time.Now().Add(time.Hour)
	_, err := repo.Create(ctx(t), &domain.PasswordReset{UserID: first, TokenHash: "same", ExpiresAt: expires})
	mustNoErr(t, err, "the first insert")

	if _, err := repo.Create(ctx(t), &domain.PasswordReset{
		UserID: second, TokenHash: "same", ExpiresAt: expires,
	}); err == nil {
		t.Error("the same token hash was stored twice")
	}
}

func TestSpendingOneLinkSpendsTheAccountsOthers(t *testing.T) {
	// Somebody who clicks "forgot" twice holds two live links. Using either one
	// has to end both, or the older mail still works after the account has been
	// recovered, which is exactly the mail an attacker would be holding.
	s := fresh(t)
	repo := NewPasswordResetRepo(s)
	userID := seedResetUser(t, s, "operator@example.com")
	other := seedResetUser(t, s, "somebody-else@example.com")

	expires := time.Now().Add(time.Hour)
	_, err := repo.Create(ctx(t), &domain.PasswordReset{UserID: userID, TokenHash: "older", ExpiresAt: expires})
	mustNoErr(t, err, "the older link")
	newer, err := repo.Create(ctx(t), &domain.PasswordReset{UserID: userID, TokenHash: "newer", ExpiresAt: expires})
	mustNoErr(t, err, "the newer link")
	_, err = repo.Create(ctx(t), &domain.PasswordReset{UserID: other, TokenHash: "elsewhere", ExpiresAt: expires})
	mustNoErr(t, err, "somebody else's link")

	mustNoErr(t, repo.MarkUsed(ctx(t), newer, time.Now()), "MarkUsed")

	for _, hash := range []string{"older", "newer"} {
		row, err := repo.FindByTokenHash(ctx(t), hash)
		mustNoErr(t, err, "FindByTokenHash "+hash)
		if row.UsedAt == nil {
			t.Errorf("%q is still live after the account's link was used", hash)
		}
		if row.Live(time.Now()) {
			t.Errorf("%q reports itself usable", hash)
		}
	}
	// And nobody else's link was touched.
	row, err := repo.FindByTokenHash(ctx(t), "elsewhere")
	mustNoErr(t, err, "FindByTokenHash elsewhere")
	if row.UsedAt != nil {
		t.Error("another account's link was spent")
	}
}

func TestExpiredAndSpentLinksArePruned(t *testing.T) {
	s := fresh(t)
	repo := NewPasswordResetRepo(s)
	userID := seedResetUser(t, s, "operator@example.com")

	_, err := repo.Create(ctx(t), &domain.PasswordReset{
		UserID: userID, TokenHash: "live", ExpiresAt: time.Now().Add(time.Hour),
	})
	mustNoErr(t, err, "the live link")
	_, err = repo.Create(ctx(t), &domain.PasswordReset{
		UserID: userID, TokenHash: "expired", ExpiresAt: time.Now().Add(-time.Hour),
	})
	mustNoErr(t, err, "the expired link")

	n, err := repo.DeleteExpired(ctx(t), time.Now())
	mustNoErr(t, err, "DeleteExpired")
	if n != 1 {
		t.Errorf("pruned %d row(s), want the expired one", n)
	}
	if _, err := repo.FindByTokenHash(ctx(t), "live"); err != nil {
		t.Errorf("the live link was pruned: %v", err)
	}
	if _, err := repo.FindByTokenHash(ctx(t), "expired"); err == nil {
		t.Error("the expired link survived")
	}
}

func TestAResetLinkGoesWithItsAccount(t *testing.T) {
	// ON DELETE CASCADE, so a removed account does not leave a live credential
	// behind pointing at a row that is gone.
	s := fresh(t)
	repo := NewPasswordResetRepo(s)
	userID := seedResetUser(t, s, "operator@example.com")

	_, err := repo.Create(ctx(t), &domain.PasswordReset{
		UserID: userID, TokenHash: "hash", ExpiresAt: time.Now().Add(time.Hour),
	})
	mustNoErr(t, err, "Create")

	if _, err := s.db.ExecContext(ctx(t), `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("deleting the account: %v", err)
	}
	if _, err := repo.FindByTokenHash(ctx(t), "hash"); err == nil {
		t.Error("the link outlived the account it belonged to")
	}
}
