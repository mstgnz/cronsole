package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// The mailed reset link.
//
// It is a bearer credential for an account, handed to whoever holds the
// message, so the properties tested here are the ones that bound it: it is
// stored hashed, it works once, it expires, and asking for one tells the
// asker nothing about who has an account.

// seedAccount creates an account through the service, the way an administrator
// would, and returns its id.
func seedAccount(t *testing.T, svc *AuthService, email, password string) int64 {
	t.Helper()

	id, err := svc.CreateUser(context.Background(), UserInput{
		Fullname: "Operator", Email: email, Password: password, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateUser = %v", err)
	}
	return id
}

func TestAResetLinkIsStoredHashedNeverInTheClear(t *testing.T) {
	// The table would otherwise be a list of live credentials: anybody who can
	// read a backup could sign in as everybody who asked for a reset.
	svc, _, resets, mail := newTestAuthServiceWithResets()
	seedAccount(t, svc, "operator@example.com", "the-first-password")

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset = %v", err)
	}

	to, raw, sends := mail.sent()
	if sends != 1 || to != "operator@example.com" {
		t.Fatalf("mail: to=%q sends=%d, want one to the account", to, sends)
	}
	if raw == "" {
		t.Fatal("no token was mailed")
	}

	resets.mu.Lock()
	defer resets.mu.Unlock()
	if len(resets.rows) != 1 {
		t.Fatalf("%d rows stored, want 1", len(resets.rows))
	}
	for _, row := range resets.rows {
		if row.TokenHash == raw {
			t.Error("the token was stored as it was mailed")
		}
		if row.TokenHash != hashResetToken(raw) {
			t.Error("the stored hash is not the hash of the mailed token")
		}
		if row.ExpiresAt.Before(time.Now()) {
			t.Error("the link was born expired")
		}
		if row.ExpiresAt.After(time.Now().Add(ResetTokenTTL + time.Minute)) {
			t.Errorf("the link lives until %v, longer than the %v it should", row.ExpiresAt, ResetTokenTTL)
		}
	}
}

func TestAnUnknownAddressIsAnsweredLikeAKnownOne(t *testing.T) {
	// The form must not answer "does this person have an account here", which
	// on an internal console is also "does this person work here".
	svc, _, _, mail := newTestAuthServiceWithResets()

	if err := svc.RequestPasswordReset(context.Background(), "nobody@example.com"); err != nil {
		t.Fatalf("an unknown address produced an error the caller could see: %v", err)
	}
	if _, _, sends := mail.sent(); sends != 0 {
		t.Errorf("%d message(s) were sent for an address with no account", sends)
	}
}

func TestADisabledAccountGetsNoLink(t *testing.T) {
	// Same answer as an unknown address, and no mail: a deactivated account is
	// how somebody who left is kept out.
	svc, repo, _, mail := newTestAuthServiceWithResets()
	id := seedAccount(t, svc, "left@example.com", "the-first-password")

	repo.mu.Lock()
	repo.users[id].Active = false
	repo.mu.Unlock()

	if err := svc.RequestPasswordReset(context.Background(), "left@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset = %v", err)
	}
	if _, _, sends := mail.sent(); sends != 0 {
		t.Error("a disabled account was mailed a way back in")
	}
}

func TestTheLinkSetsThePasswordAndEndsEverySession(t *testing.T) {
	svc, repo, _, mail := newTestAuthServiceWithResets()
	id := seedAccount(t, svc, "operator@example.com", "the-first-password")

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset = %v", err)
	}
	_, raw, _ := mail.sent()

	if err := svc.CompletePasswordReset(context.Background(), raw, "the-replacement-password"); err != nil {
		t.Fatalf("CompletePasswordReset = %v", err)
	}

	repo.mu.Lock()
	user := *repo.users[id]
	repo.mu.Unlock()

	if !auth.ComparePassword(user.Password, "the-replacement-password") {
		t.Error("the new password does not verify")
	}
	if auth.ComparePassword(user.Password, "the-first-password") {
		t.Error("the old password still works")
	}
	// A reset is how somebody recovers an account that may already be in
	// someone else's hands, so the sessions that hand is holding have to end.
	if user.TokensValidAfter == nil {
		t.Error("the sessions open before the reset were left working")
	}
}

func TestALinkWorksOnce(t *testing.T) {
	svc, _, _, mail := newTestAuthServiceWithResets()
	seedAccount(t, svc, "operator@example.com", "the-first-password")

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset = %v", err)
	}
	_, raw, _ := mail.sent()

	if err := svc.CompletePasswordReset(context.Background(), raw, "the-replacement-password"); err != nil {
		t.Fatalf("the first use failed: %v", err)
	}
	err := svc.CompletePasswordReset(context.Background(), raw, "a-third-password-entirely")
	if !errors.Is(err, ErrResetLinkInvalid) {
		t.Errorf("the second use gave %v, want ErrResetLinkInvalid", err)
	}
}

func TestUsingALinkRetiresTheOtherOutstandingOnes(t *testing.T) {
	// Somebody who asks twice gets two live links. Spending one has to spend
	// the other, or an older mail sitting in a stolen inbox still works after
	// the owner has recovered the account.
	svc, _, _, mail := newTestAuthServiceWithResets()
	seedAccount(t, svc, "operator@example.com", "the-first-password")

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, first, _ := mail.sent()

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err != nil {
		t.Fatalf("second request: %v", err)
	}
	_, second, _ := mail.sent()
	if first == second {
		t.Fatal("the same token was issued twice")
	}

	if err := svc.CompletePasswordReset(context.Background(), second, "the-replacement-password"); err != nil {
		t.Fatalf("using the newer link failed: %v", err)
	}
	err := svc.CompletePasswordReset(context.Background(), first, "yet-another-password")
	if !errors.Is(err, ErrResetLinkInvalid) {
		t.Errorf("the older link gave %v, want it spent as well", err)
	}
}

func TestAnExpiredLinkIsRefused(t *testing.T) {
	svc, _, resets, mail := newTestAuthServiceWithResets()
	seedAccount(t, svc, "operator@example.com", "the-first-password")

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset = %v", err)
	}
	_, raw, _ := mail.sent()

	// Wind the clock forward by moving the row rather than sleeping an hour.
	resets.mu.Lock()
	for _, row := range resets.rows {
		row.ExpiresAt = time.Now().Add(-time.Minute)
	}
	resets.mu.Unlock()

	err := svc.CompletePasswordReset(context.Background(), raw, "the-replacement-password")
	if !errors.Is(err, ErrResetLinkInvalid) {
		t.Errorf("an expired link gave %v, want ErrResetLinkInvalid", err)
	}
}

func TestAnUnknownTokenIsRefusedTheSameWay(t *testing.T) {
	svc, _, _, _ := newTestAuthServiceWithResets()

	for _, token := range []string{"", "   ", "not-a-token", auth.RandomHex(32)} {
		err := svc.CompletePasswordReset(context.Background(), token, "a-long-enough-password")
		if !errors.Is(err, ErrResetLinkInvalid) {
			t.Errorf("token %q gave %v, want ErrResetLinkInvalid", token, err)
		}
	}
}

func TestAWeakPasswordDoesNotSpendTheLink(t *testing.T) {
	// Somebody who types a password the rules refuse should be able to try
	// again with the mail they already have, rather than starting over.
	svc, _, _, mail := newTestAuthServiceWithResets()
	seedAccount(t, svc, "operator@example.com", "the-first-password")

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset = %v", err)
	}
	_, raw, _ := mail.sent()

	var ve *ValidationError
	if err := svc.CompletePasswordReset(context.Background(), raw, "short"); !errors.As(err, &ve) {
		t.Fatalf("a short password gave %v, want a validation error", err)
	}
	if err := svc.CompletePasswordReset(context.Background(), raw, "a-long-enough-password"); err != nil {
		t.Errorf("the link was spent by the refused attempt: %v", err)
	}
}

func TestResetIsRefusedWhereItCannotBeDelivered(t *testing.T) {
	// A deployment with no mail server must say so rather than claim a message
	// is on its way.
	repo := newUserRepo()
	svc := NewAuthService(repo, nil, nil, nil, &fakeGrants{}, testLogger())

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); !errors.Is(err, ErrResetUnavailable) {
		t.Errorf("RequestPasswordReset = %v, want ErrResetUnavailable", err)
	}
	if err := svc.CompletePasswordReset(context.Background(), "anything", "a-long-enough-password"); !errors.Is(err, ErrResetUnavailable) {
		t.Errorf("CompletePasswordReset = %v, want ErrResetUnavailable", err)
	}
}

func TestNothingIsMailedWhenTheLinkCannotBeRecorded(t *testing.T) {
	// The mail is the credential. Sending one that was never written down
	// would hand out a link nothing can honour, and the person would be told
	// to wait for a message that cannot work.
	svc, _, resets, mail := newTestAuthServiceWithResets()
	seedAccount(t, svc, "operator@example.com", "the-first-password")
	resets.createErr = errors.New("pq: could not write")

	if err := svc.RequestPasswordReset(context.Background(), "operator@example.com"); err == nil {
		t.Error("the failure was swallowed")
	}
	if _, _, sends := mail.sent(); sends != 0 {
		t.Error("a link was mailed that nothing recorded")
	}
}

func TestTheMailCarriesALinkSomebodyCanClick(t *testing.T) {
	// The notifier builds the URL, because it is the one part of the system
	// that knows the address this deployment answers at.
	sender := &captureSender{}
	notifier := NewNotifier(sender, testLogger(), "https://cron.example.com/")

	notifier.PasswordReset("operator@example.com", "the-raw-token", time.Now().Add(time.Hour))
	drain(notifier)

	if sender.count() != 1 {
		t.Fatalf("%d message(s) sent, want 1", sender.count())
	}
	sent := sender.sent[0]
	if len(sent.To) != 1 || sent.To[0] != "operator@example.com" {
		t.Errorf("sent to %v", sent.To)
	}
	if !strings.Contains(sent.Body, "https://cron.example.com/reset?token=the-raw-token") {
		t.Errorf("the body carries no usable link:\n%s", sent.Body)
	}
	// Said plainly, because somebody who did not ask for this needs to know
	// whether they have to do anything.
	if !strings.Contains(sent.Body, "not you") {
		t.Errorf("the message does not say what to do if it was not you:\n%s", sent.Body)
	}
}

func TestNoLinkIsMailedWithNowhereToPointIt(t *testing.T) {
	// APP_URL unset. A message carrying a bare path helps nobody, and it would
	// still have spent a real token.
	sender := &captureSender{}
	notifier := NewNotifier(sender, testLogger(), "")

	notifier.PasswordReset("operator@example.com", "the-raw-token", time.Now().Add(time.Hour))
	drain(notifier)

	if sender.count() != 0 {
		t.Errorf("a message went out with no host to point at: %+v", sender.sent)
	}
}
