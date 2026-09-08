package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
	"github.com/mstgnz/cronsole/v2/pkg/token"
)

// AuthService owns accounts and sessions.
type AuthService struct {
	users  domain.UserRepository
	resets domain.PasswordResetRepository
	mail   ResetMailer
	tokens *token.Issuer
	grants GrantRevoker
	log    *applog.Logger
}

// GrantRevoker removes everything an account was granted.
//
// A narrow interface rather than the whole authorization store, because this is
// the only thing account management needs from it, and a service that can read
// the role catalogue is a service that will eventually write to it.
type GrantRevoker interface {
	RevokeUser(ctx context.Context, userID int64) error
}

// ResetMailer delivers a password reset link.
//
// Narrow, and an interface rather than the notifier itself, because this is the
// only message account management sends and because a deployment with no mail
// server configured must still start: the sender is allowed to be nil, and
// then a reset simply cannot be requested.
type ResetMailer interface {
	PasswordReset(to, link string, expires time.Time)
}

// NewAuthService wires the service.
//
// resets and mail may be nil, which is what a test that does not exercise the
// reset flow passes. RequestPasswordReset refuses rather than panicking.
func NewAuthService(users domain.UserRepository, resets domain.PasswordResetRepository,
	mail ResetMailer, tokens *token.Issuer, grants GrantRevoker, log *applog.Logger) *AuthService {

	return &AuthService{users: users, resets: resets, mail: mail,
		tokens: tokens, grants: grants, log: log}
}

// Login verifies credentials and issues a token.
//
// A wrong address and a wrong password are indistinguishable to the caller,
// and they cost the same time: the hash comparison runs against a fixed dummy
// hash when the account does not exist. Skipping it would let an attacker
// enumerate registered addresses from the response time alone.
func (s *AuthService) Login(ctx context.Context, email, password string) (*domain.User, string, error) {
	email = strings.TrimSpace(strings.ToLower(email))

	user, err := s.users.GetByEmail(ctx, email)
	if errors.Is(err, repository.ErrNotFound) {
		auth.ComparePassword(auth.DummyHash, password)
		return nil, "", ErrInvalidLogin
	}
	if err != nil {
		return nil, "", err
	}

	if !auth.ComparePassword(user.Password, password) {
		return nil, "", ErrInvalidLogin
	}
	if !user.Active {
		return nil, "", ErrInactiveUser
	}

	signed, err := s.tokens.Issue(user.ID)
	if err != nil {
		return nil, "", err
	}
	if err := s.users.TouchLogin(ctx, user.ID, time.Now()); err != nil {
		// Not fatal to the login: a missing last_login is cosmetic, refusing a
		// valid sign in over it is not.
		s.log.Warn("auth: last login could not be recorded", err.Error(), "user_id", user.ID)
	}
	return user, signed, nil
}

// IssueToken signs a session for an account that has already been established.
//
// Used by setup, which has just created the account and has no password to
// verify against it. Deliberately narrow: it takes a user the caller already
// holds, so there is no path here that turns an address into a session.
func (s *AuthService) IssueToken(user *domain.User) (string, error) {
	if user == nil {
		return "", ErrForbidden
	}
	return s.tokens.Issue(user.ID)
}

// Authenticate resolves a raw token to the account it belongs to.
func (s *AuthService) Authenticate(ctx context.Context, raw string) (*domain.User, error) {
	userID, issuedAt, err := s.tokens.Verify(raw)
	if err != nil {
		return nil, ErrForbidden
	}

	user, err := s.users.GetByID(ctx, userID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	if !user.Active || user.TokenRetired(issuedAt) {
		return nil, ErrForbidden
	}
	return user, nil
}

// Logout retires every token issued to the account before now.
//
// A JWT cannot be withdrawn once signed, so ending a session means moving this
// cut-off, not deleting anything.
func (s *AuthService) Logout(ctx context.Context, userID int64) error {
	return s.users.InvalidateTokens(ctx, userID, time.Now())
}

// ChangePassword verifies the current password before setting a new one, and
// ends every existing session.
func (s *AuthService) ChangePassword(ctx context.Context, userID int64, current, next string) error {
	user, err := s.users.GetByID(ctx, userID)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !auth.ComparePassword(user.Password, current) {
		return (&ValidationError{}).Add("current_password", "does not match")
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	return s.users.UpdatePassword(ctx, userID, auth.HashAndSalt(next), time.Now())
}

// ResetPassword sets a password without knowing the old one. Admin only, and
// it also ends the target account's sessions.
func (s *AuthService) ResetPassword(ctx context.Context, userID int64, next string) error {
	if err := validatePassword(next); err != nil {
		return err
	}
	return s.users.UpdatePassword(ctx, userID, auth.HashAndSalt(next), time.Now())
}

// ResetTokenTTL is how long a mailed link works.
//
// An hour, following OWASP: the link is a bearer credential for the account, and
// the window in which it works is the window in which a forwarded mail, a shared
// screen or a mail server's log is worth stealing. Long enough that somebody can
// finish a coffee first.
const ResetTokenTTL = time.Hour

// ErrResetUnavailable means this deployment cannot send the mail, so there is no
// point pretending a link was sent.
var ErrResetUnavailable = errors.New("password reset is not available on this deployment")

// ErrResetLinkInvalid covers unknown, expired, already used, and belonging to a
// disabled account. One sentinel on purpose: telling those apart tells whoever
// is guessing which half of the guess was right.
var ErrResetLinkInvalid = errors.New("this link is no longer valid")

// RequestPasswordReset mails a reset link, if there is an account to mail.
//
// It answers the same way whether or not the address is registered, and the
// caller must render the same page either way. Anything else turns this form
// into a way to ask "does this person have an account here", which for an
// internal console is also "does this person work here".
//
// The mail goes onto the notifier's queue rather than an SMTP handshake on the
// request, so a slow mail server cannot hold the form open or make the answer
// take measurably longer for an address that exists.
func (s *AuthService) RequestPasswordReset(ctx context.Context, email string) error {
	if s.resets == nil || s.mail == nil {
		return ErrResetUnavailable
	}

	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil
	}

	user, err := s.users.GetByEmail(ctx, email)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		// No account. Logged so an operator can see the attempt, and answered
		// exactly like the success case.
		s.log.Warn("auth: password reset asked for an unknown address", email)
		return nil
	case err != nil:
		return err
	}
	if !user.Active {
		s.log.Warn("auth: password reset asked for a disabled account", email, "user_id", user.ID)
		return nil
	}

	// 32 bytes, hex. The link is the credential, so it is generated here and
	// stored only as a hash; nothing can read it back out of the database.
	raw := auth.RandomHex(32)
	expires := time.Now().Add(ResetTokenTTL)
	if _, err := s.resets.Create(ctx, &domain.PasswordReset{
		UserID:    user.ID,
		TokenHash: hashResetToken(raw),
		ExpiresAt: expires,
	}); err != nil {
		return err
	}

	s.mail.PasswordReset(user.Email, raw, expires)
	s.log.Warn("auth: a password reset link was sent", user.Email, "user_id", user.ID)
	return nil
}

// CompletePasswordReset spends a link and sets the new password.
//
// The password is validated BEFORE the link is spent, so somebody who types a
// password the rules refuse does not also lose their only link and have to
// start again from the mail.
func (s *AuthService) CompletePasswordReset(ctx context.Context, rawToken, next string) error {
	if s.resets == nil {
		return ErrResetUnavailable
	}
	if strings.TrimSpace(rawToken) == "" {
		return ErrResetLinkInvalid
	}
	if err := validatePassword(next); err != nil {
		return err
	}

	reset, err := s.resets.FindByTokenHash(ctx, hashResetToken(rawToken))
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return ErrResetLinkInvalid
	case err != nil:
		return err
	}
	if !reset.Live(time.Now()) {
		return ErrResetLinkInvalid
	}

	user, err := s.users.GetByID(ctx, reset.UserID)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return ErrResetLinkInvalid
	case err != nil:
		return err
	}
	if !user.Active {
		return ErrResetLinkInvalid
	}

	// The link is spent first. If the password write then fails the person
	// asks for another mail, which is an inconvenience; the other order leaves
	// a used link live, which is an account.
	now := time.Now()
	if err := s.resets.MarkUsed(ctx, reset.ID, now); err != nil {
		return err
	}
	// UpdatePassword also moves tokens_valid_after, so every session opened
	// before this moment stops working. That is the point of a reset: whoever
	// was signed in with the old password is signed out.
	if err := s.users.UpdatePassword(ctx, user.ID, auth.HashAndSalt(next), now); err != nil {
		return err
	}

	s.log.Warn("auth: a password was reset through a mailed link", user.Email, "user_id", user.ID)
	return nil
}

// hashResetToken is plain sha256, not bcrypt, and deliberately.
//
// The token is 32 random bytes rather than something a person chose, so there
// is nothing to brute force and no reason to pay bcrypt's cost on a lookup that
// has to be fast. Same reasoning as the project API keys.
func hashResetToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// UserInput is what an admin may set on an account. Password is separate: it
// is hashed here and never round trips through a form.
type UserInput struct {
	Fullname string `json:"fullname"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Password string `json:"password,omitempty"`
	IsAdmin  bool   `json:"is_admin"`
	Active   bool   `json:"active"`
}

// CreateUser adds an account.
func (s *AuthService) CreateUser(ctx context.Context, in UserInput) (int64, error) {
	v := &ValidationError{}
	in.Fullname = strings.TrimSpace(in.Fullname)
	in.Email = strings.TrimSpace(strings.ToLower(in.Email))

	if in.Fullname == "" {
		v.Add("fullname", "required")
	}
	if !strings.Contains(in.Email, "@") || len(in.Email) < 5 {
		v.Add("email", "not a valid address")
	}
	if err := validatePassword(in.Password); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			v.Errors = append(v.Errors, ve.Errors...)
		}
	}
	if !v.OK() {
		return 0, v
	}

	id, err := s.users.Create(ctx, &domain.User{
		Fullname: in.Fullname,
		Email:    in.Email,
		Password: auth.HashAndSalt(in.Password),
		Phone:    strings.TrimSpace(in.Phone),
		IsAdmin:  in.IsAdmin,
		Active:   in.Active,
	})
	if errors.Is(err, repository.ErrDuplicate) {
		return 0, (&ValidationError{}).Add("email", "an account with this address already exists")
	}
	return id, err
}

// UpdateUser writes an account's profile.
func (s *AuthService) UpdateUser(ctx context.Context, id int64, in UserInput) error {
	user, err := s.users.GetByID(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	user.Fullname = strings.TrimSpace(in.Fullname)
	user.Email = strings.TrimSpace(strings.ToLower(in.Email))
	user.Phone = strings.TrimSpace(in.Phone)
	user.IsAdmin = in.IsAdmin
	user.Active = in.Active

	if user.Fullname == "" || !strings.Contains(user.Email, "@") {
		return (&ValidationError{}).Add("email", "name and a valid address are required")
	}

	err = s.users.UpdateProfile(ctx, user)
	if errors.Is(err, repository.ErrDuplicate) {
		return (&ValidationError{}).Add("email", "an account with this address already exists")
	}
	return err
}

// DeleteUser deactivates an account, ends its sessions and drops its access.
//
// The grants go first. An account is soft deleted, so its rows survive; if the
// address were ever reused, or the flag flipped back by hand, whatever it could
// reach a year ago would come back with it.
func (s *AuthService) DeleteUser(ctx context.Context, id int64) error {
	if s.grants != nil {
		if err := s.grants.RevokeUser(ctx, id); err != nil {
			return err
		}
	}
	if err := s.users.SoftDelete(ctx, id, time.Now()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// ListUsers pages through accounts.
func (s *AuthService) ListUsers(ctx context.Context, search string, offset, limit int) ([]domain.User, int64, error) {
	return s.users.List(ctx, search, offset, limit)
}

// GetUser reads one account.
func (s *AuthService) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	user, err := s.users.GetByID(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	return user, err
}

// ErrSetupDone is the answer when the first account already exists.
//
// Deliberately not a validation error: nothing about the request was wrong, the
// window simply closed.
var ErrSetupDone = errors.New("the first administrator already exists")

// NeedsSetup reports whether this deployment has no account yet.
//
// Read fresh by the caller on every request while it is true. Caching a true
// answer would keep the setup screen open after somebody used it, which is the
// one direction that matters: caching false only hides a screen that should be
// hidden.
func (s *AuthService) NeedsSetup(ctx context.Context) (bool, error) {
	count, err := s.users.Count(ctx)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// CompleteSetup creates the first administrator.
//
// This is the one write in the service that no authenticated caller stands
// behind, so the guard is not "is this person allowed" but "is the table
// empty", and that question is answered by the INSERT itself rather than by a
// count read beforehand. A count would be a race whose prize is an
// administrator account.
func (s *AuthService) CompleteSetup(ctx context.Context, in UserInput) (*domain.User, error) {
	v := &ValidationError{}
	in.Fullname = strings.TrimSpace(in.Fullname)
	in.Email = strings.TrimSpace(strings.ToLower(in.Email))

	if in.Fullname == "" {
		v.Add("fullname", "required")
	}
	if !strings.Contains(in.Email, "@") || len(in.Email) < 5 {
		v.Add("email", "not a valid address")
	}
	if err := validatePassword(in.Password); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			v.Errors = append(v.Errors, ve.Errors...)
		}
	}
	if !v.OK() {
		return nil, v
	}

	// IsAdmin and Active are set by the repository, not taken from the input.
	// There is no caller to trust here, so the shape of the first account is
	// decided in code.
	id, err := s.users.CreateFirstAdmin(ctx, &domain.User{
		Fullname: in.Fullname,
		Email:    in.Email,
		Password: auth.HashAndSalt(in.Password),
		Phone:    strings.TrimSpace(in.Phone),
	})
	switch {
	case errors.Is(err, repository.ErrForbidden):
		return nil, ErrSetupDone
	case errors.Is(err, repository.ErrDuplicate):
		// The table was empty a moment ago and now holds this address, which
		// means somebody else completed setup between the two. Same answer.
		return nil, ErrSetupDone
	case err != nil:
		return nil, err
	}

	s.log.Warn("setup: the first administrator was created", in.Email, "user_id", id)
	return s.users.GetByID(ctx, id)
}

// minPasswordLength follows NIST SP 800-63B: length is what matters, and
// composition rules mostly produce predictable substitutions.
const minPasswordLength = 10

func validatePassword(password string) error {
	if len([]rune(password)) < minPasswordLength {
		return (&ValidationError{}).Add("password",
			"must be at least "+itoa(minPasswordLength)+" characters")
	}
	if len(password) > 512 {
		// bcrypt only reads the first 72 bytes anyway; a huge input is only a
		// way to make the hash expensive.
		return (&ValidationError{}).Add("password", "is too long")
	}
	return nil
}
