package mailer

import (
	"errors"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/config"
	"github.com/mstgnz/cronsole/v2/pkg/mail"
)

func TestNewCarriesEverySetting(t *testing.T) {
	// A setting silently dropped here is a mail server that is configured and
	// does not work, and the symptom is an alert that never arrives.
	cfg := config.Mail{
		Host:     "smtp.example.com",
		Port:     "465",
		User:     "cron",
		Pass:     "secret",
		From:     "cron@example.com",
		FromName: "Cronsole",
	}

	got := New(cfg).sender
	want := mail.Sender{
		Host: "smtp.example.com",
		Port: "465",
		User: "cron",
		Pass: "secret",
		From: "cron@example.com",
		Name: "Cronsole",
	}
	if got != want {
		t.Errorf("sender = %+v, want %+v", got, want)
	}
}

func TestSendReachesTheSender(t *testing.T) {
	// Nothing to talk to, so the only observable is the error, which is enough
	// to prove the call is not being swallowed by the adapter.
	s := New(config.Mail{})
	if err := s.Send([]string{"ops@example.com"}, "subject", "body"); !errors.Is(err, mail.ErrNotConfigured) {
		t.Errorf("Send = %v, want ErrNotConfigured", err)
	}
}
