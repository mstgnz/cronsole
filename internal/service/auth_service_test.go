package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
	"github.com/mstgnz/cronsole/v2/pkg/token"
)

// memUserRepo is an in-memory account store.
type memUserRepo struct {
	mu     sync.Mutex
	users  map[int64]*domain.User
	nextID int64
}

func newUserRepo() *memUserRepo {
	return &memUserRepo{users: map[int64]*domain.User{}, nextID: 1}
}

func (m *memUserRepo) GetByID(_ context.Context, id int64) (*domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		copied := *u
		return &copied, nil
	}
	return nil, repository.ErrNotFound
}

func (m *memUserRepo) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			copied := *u
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m *memUserRepo) List(context.Context, string, int, int) ([]domain.User, int64, error) {
	return nil, 0, nil
}

func (m *memUserRepo) Count(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.users)), nil
}

func (m *memUserRepo) Create(_ context.Context, u *domain.User) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.users {
		if strings.EqualFold(existing.Email, u.Email) {
			return 0, repository.ErrDuplicate
		}
	}
	id := m.nextID
	m.nextID++
	copied := *u
	copied.ID = id
	copied.CreatedAt = time.Now()
	m.users[id] = &copied
	return id, nil
}

// CreateFirstAdmin refuses the moment any account exists, and does the check
// under the same lock as the insert. The real one is a single statement for the
// same reason: two callers both finding the table empty is how a stranger
// becomes the administrator.
func (m *memUserRepo) CreateFirstAdmin(_ context.Context, u *domain.User) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.users) > 0 {
		return 0, repository.ErrForbidden
	}
	id := m.nextID
	m.nextID++
	copied := *u
	copied.ID = id
	copied.IsAdmin, copied.Active = true, true
	copied.CreatedAt = time.Now()
	m.users[id] = &copied
	return id, nil
}

func (m *memUserRepo) UpdateProfile(_ context.Context, u *domain.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.users[u.ID]
	if !ok {
		return repository.ErrNotFound
	}
	stored.Fullname, stored.Email, stored.Phone = u.Fullname, u.Email, u.Phone
	stored.IsAdmin, stored.Active = u.IsAdmin, u.Active
	return nil
}

func (m *memUserRepo) UpdatePassword(_ context.Context, id int64, hash string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.Password = hash
		u.TokensValidAfter = &at
	}
	return nil
}

func (m *memUserRepo) InvalidateTokens(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.TokensValidAfter = &at
	}
	return nil
}

func (m *memUserRepo) TouchLogin(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.LastLogin = &at
	}
	return nil
}

func (m *memUserRepo) SoftDelete(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.Active = false
		u.TokensValidAfter = &at
	}
	return nil
}

func newTestAuthService() (*AuthService, *memUserRepo) {
	svc, repo, _, _ := newTestAuthServiceWithResets()
	return svc, repo
}

// newTestAuthServiceWithResets is the same service with the password reset
// parts attached, for the tests that exercise the mailed link.
func newTestAuthServiceWithResets() (*AuthService, *memUserRepo, *memResetRepo, *fakeResetMailer) {
	repo := newUserRepo()
	resets := &memResetRepo{rows: map[int64]*domain.PasswordReset{}}
	mail := &fakeResetMailer{}
	svc := NewAuthService(repo, resets, mail, token.NewIssuer("a-test-secret-long-enough-to-sign"),
		&fakeGrants{}, testLogger())
	return svc, repo, resets, mail
}

// memResetRepo is the reset table in memory.
type memResetRepo struct {
	mu     sync.Mutex
	rows   map[int64]*domain.PasswordReset
	nextID int64
	// createErr makes the write fail, for the path where the link cannot be
	// recorded and therefore must not be mailed.
	createErr error
}

func (m *memResetRepo) Create(_ context.Context, r *domain.PasswordReset) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.createErr != nil {
		return 0, m.createErr
	}
	m.nextID++
	copied := *r
	copied.ID = m.nextID
	copied.CreatedAt = time.Now()
	m.rows[copied.ID] = &copied
	return copied.ID, nil
}

func (m *memResetRepo) FindByTokenHash(_ context.Context, hash string) (*domain.PasswordReset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.rows {
		if r.TokenHash == hash {
			copied := *r
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m *memResetRepo) MarkUsed(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	target, ok := m.rows[id]
	if !ok {
		return repository.ErrNotFound
	}
	for _, r := range m.rows {
		if r.UserID == target.UserID && r.UsedAt == nil {
			spent := at
			r.UsedAt = &spent
		}
	}
	return nil
}

func (m *memResetRepo) DeleteExpired(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var n int64
	for id, r := range m.rows {
		if r.UsedAt != nil || r.ExpiresAt.Before(before) {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

// fakeResetMailer records what would have been sent.
type fakeResetMailer struct {
	mu      sync.Mutex
	to      string
	token   string
	expires time.Time
	sends   int
}

func (f *fakeResetMailer) PasswordReset(to, rawToken string, expires time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.to, f.token, f.expires = to, rawToken, expires
	f.sends++
}

func (f *fakeResetMailer) sent() (string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.to, f.token, f.sends
}

func TestLoginAndAuthenticate(t *testing.T) {
	svc, repo := newTestAuthService()
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, UserInput{
		Fullname: "Operator", Email: "OP@example.com", Password: "correct-horse-battery",
		Active: true,
	}); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// The address is matched without regard to case, because the unique index
	// is case insensitive too. A mismatch between the two would let a second
	// account exist under a different casing.
	user, signed, err := svc.Login(ctx, "op@EXAMPLE.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if user.Email != "op@example.com" {
		t.Errorf("email = %q, want it stored lower case", user.Email)
	}

	authed, err := svc.Authenticate(ctx, signed)
	if err != nil {
		t.Fatalf("authenticate failed: %v", err)
	}
	if authed.ID != user.ID {
		t.Errorf("authenticated %d, want %d", authed.ID, user.ID)
	}
	if stored, _ := repo.GetByID(ctx, user.ID); stored.LastLogin == nil {
		t.Error("the sign in was not recorded")
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	// A wrong address and a wrong password must give the same answer. Telling
	// them apart lets an attacker enumerate registered addresses.
	svc, _ := newTestAuthService()
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, UserInput{
		Fullname: "Operator", Email: "op@example.com", Password: "correct-horse-battery", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	_, _, unknownErr := svc.Login(ctx, "nobody@example.com", "whatever-long-enough")
	_, _, wrongPassErr := svc.Login(ctx, "op@example.com", "wrong-password-here")

	if !errors.Is(unknownErr, ErrInvalidLogin) || !errors.Is(wrongPassErr, ErrInvalidLogin) {
		t.Fatalf("errors differ: unknown=%v wrong=%v", unknownErr, wrongPassErr)
	}
	if unknownErr.Error() != wrongPassErr.Error() {
		t.Error("the two failures produced different messages")
	}
}

func TestInactiveAccountCannotSignIn(t *testing.T) {
	svc, _ := newTestAuthService()
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, UserInput{
		Fullname: "Gone", Email: "gone@example.com", Password: "correct-horse-battery", Active: false,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Login(ctx, "gone@example.com", "correct-horse-battery"); !errors.Is(err, ErrInactiveUser) {
		t.Errorf("login gave %v, want ErrInactiveUser", err)
	}
}

func TestLogoutRetiresExistingTokens(t *testing.T) {
	// A JWT cannot be withdrawn once signed. Moving the cut-off is the only
	// thing that makes signing out actually end a session.
	svc, _ := newTestAuthService()
	ctx := context.Background()

	id, err := svc.CreateUser(ctx, UserInput{
		Fullname: "Operator", Email: "op@example.com", Password: "correct-horse-battery", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, signed, err := svc.Login(ctx, "op@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, signed); err != nil {
		t.Fatalf("the fresh token was rejected: %v", err)
	}

	// The cut-off has second precision while the token's iat does too, and the
	// comparison is strict, so a token minted in the same second counts as
	// retired. Waiting past the boundary keeps the test about the mechanism.
	time.Sleep(1100 * time.Millisecond)
	if err := svc.Logout(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, signed); !errors.Is(err, ErrForbidden) {
		t.Errorf("a token issued before logout was still accepted: %v", err)
	}
}

func TestChangePasswordVerifiesTheCurrentOne(t *testing.T) {
	svc, repo := newTestAuthService()
	ctx := context.Background()

	id, err := svc.CreateUser(ctx, UserInput{
		Fullname: "Operator", Email: "op@example.com", Password: "correct-horse-battery", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.ChangePassword(ctx, id, "not-the-password", "a-new-long-password"); err == nil {
		t.Error("the password was changed without the current one")
	}
	if err := svc.ChangePassword(ctx, id, "correct-horse-battery", "short"); err == nil {
		t.Error("a short password was accepted")
	}
	if err := svc.ChangePassword(ctx, id, "correct-horse-battery", "a-new-long-password"); err != nil {
		t.Fatalf("the change failed: %v", err)
	}

	stored, _ := repo.GetByID(ctx, id)
	if !auth.ComparePassword(stored.Password, "a-new-long-password") {
		t.Error("the new password does not verify")
	}
	// The change has to end every session, which is what the cut-off does.
	if stored.TokensValidAfter == nil {
		t.Error("a password change did not retire existing tokens")
	}
}

func TestCreateUserValidation(t *testing.T) {
	svc, _ := newTestAuthService()
	ctx := context.Background()

	_, err := svc.CreateUser(ctx, UserInput{Fullname: "", Email: "nope", Password: "short"})
	got := fieldErrors(t, err)
	for _, field := range []string{"fullname", "email", "password"} {
		if _, ok := got[field]; !ok {
			t.Errorf("no failure reported for %q; got %v", field, got)
		}
	}

	if _, err := svc.CreateUser(ctx, UserInput{
		Fullname: "A", Email: "a@example.com", Password: "long-enough-password", Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateUser(ctx, UserInput{
		Fullname: "B", Email: "A@example.com", Password: "long-enough-password", Active: true,
	}); err == nil {
		t.Error("a duplicate address was accepted")
	}
}

func TestSetupIsNeededOnlyWhileThereIsNoAccount(t *testing.T) {
	svc, _ := newTestAuthService()
	ctx := context.Background()

	needed, err := svc.NeedsSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !needed {
		t.Fatal("a fresh deployment does not report that it needs setting up")
	}

	if _, err := svc.CompleteSetup(ctx, UserInput{
		Fullname: "Mesut", Email: "admin@example.com", Password: "a-long-enough-password",
	}); err != nil {
		t.Fatalf("CompleteSetup = %v", err)
	}

	needed, err = svc.NeedsSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if needed {
		t.Error("setup is still reported as needed after the account exists")
	}
}

func TestTheFirstAccountIsAnActiveAdministrator(t *testing.T) {
	// Not taken from the input: there is no caller to trust on this screen, so
	// the shape of the first account is decided in code.
	svc, repo := newTestAuthService()
	ctx := context.Background()

	user, err := svc.CompleteSetup(ctx, UserInput{
		Fullname: "Mesut", Email: "Admin@Example.com", Password: "a-long-enough-password",
	})
	if err != nil {
		t.Fatalf("CompleteSetup = %v", err)
	}
	if !user.IsAdmin || !user.Active {
		t.Errorf("the first account is %+v, want an active administrator", user)
	}
	// The address is stored folded, like every other account, or signing in
	// with a different case would fail.
	if user.Email != "admin@example.com" {
		t.Errorf("email = %q, want it folded", user.Email)
	}

	stored, err := repo.GetByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Password == "a-long-enough-password" {
		t.Fatal("the password was stored in clear text")
	}
	if _, _, err := svc.Login(ctx, "admin@example.com", "a-long-enough-password"); err != nil {
		t.Errorf("the account created by setup cannot sign in: %v", err)
	}
}

func TestSetupClosesOnceItHasBeenUsed(t *testing.T) {
	// The window this screen opens is the one time an unauthenticated caller
	// can create an administrator. It has to shut, and the refusal is not a
	// validation failure: nothing about the second request was wrong.
	svc, _ := newTestAuthService()
	ctx := context.Background()

	if _, err := svc.CompleteSetup(ctx, UserInput{
		Fullname: "First", Email: "first@example.com", Password: "a-long-enough-password",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := svc.CompleteSetup(ctx, UserInput{
		Fullname: "Second", Email: "second@example.com", Password: "a-long-enough-password",
	})
	if !errors.Is(err, ErrSetupDone) {
		t.Errorf("the second setup = %v, want ErrSetupDone", err)
	}

	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Error("the refusal is a validation error, which reads as though the form was wrong")
	}
}

func TestSetupValidatesWhatItIsGiven(t *testing.T) {
	// Every failure at once, like every other form. An administrator account
	// with a four character password is not a thing to create quietly.
	svc, _ := newTestAuthService()

	_, err := svc.CompleteSetup(context.Background(), UserInput{
		Fullname: "", Email: "not-an-address", Password: "short",
	})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("CompleteSetup = %v, want a validation error", err)
	}

	fields := map[string]bool{}
	for _, e := range ve.Errors {
		fields[e.Field] = true
	}
	for _, want := range []string{"fullname", "email", "password"} {
		if !fields[want] {
			t.Errorf("the answer does not mention %q: %+v", want, ve.Errors)
		}
	}
}

func TestOnlyOneCallerWinsTheFirstAccount(t *testing.T) {
	// Two requests reaching an empty table at once. The check and the insert
	// are one operation, so exactly one of them can win; the loser gets a
	// stranger's console otherwise.
	svc, _ := newTestAuthService()

	const callers = 8
	var wg sync.WaitGroup
	results := make(chan error, callers)

	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			_, err := svc.CompleteSetup(context.Background(), UserInput{
				Fullname: "Claimant",
				Email:    "claimant" + itoa(n) + "@example.com",
				Password: "a-long-enough-password",
			})
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	won := 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrSetupDone):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d callers created an administrator, want exactly 1", won)
	}
}

func TestIssueTokenNeedsAnAccount(t *testing.T) {
	// Deliberately narrow: it takes a user the caller already holds, so there
	// is no path here that turns an address into a session.
	svc, _ := newTestAuthService()

	if _, err := svc.IssueToken(nil); err == nil {
		t.Error("a token was issued for nobody")
	}

	user, err := svc.CompleteSetup(context.Background(), UserInput{
		Fullname: "Mesut", Email: "admin@example.com", Password: "a-long-enough-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := svc.IssueToken(user)
	if err != nil {
		t.Fatalf("IssueToken = %v", err)
	}
	back, err := svc.Authenticate(context.Background(), signed)
	if err != nil || back.ID != user.ID {
		t.Errorf("the issued token does not authenticate: %v", err)
	}
}

func TestAuthenticateRejectsADeactivatedAccount(t *testing.T) {
	svc, _ := newTestAuthService()
	ctx := context.Background()

	id, err := svc.CreateUser(ctx, UserInput{
		Fullname: "Operator", Email: "op@example.com", Password: "correct-horse-battery", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, signed, err := svc.Login(ctx, "op@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, signed); !errors.Is(err, ErrForbidden) {
		t.Errorf("a deactivated account's token was accepted: %v", err)
	}
}
