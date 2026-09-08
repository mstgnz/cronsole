// Package mailer adapts the SMTP sender to the interface the services expect.
//
// It exists so nothing in internal/service knows what SMTP is. Swapping the
// transport for an API based provider is then one type, not a search through
// the business rules for send calls.
package mailer

import (
	"github.com/mstgnz/cronsole/v2/internal/config"
	"github.com/mstgnz/cronsole/v2/pkg/mail"
)

// SMTP sends through a mail server.
type SMTP struct {
	sender mail.Sender
}

// New builds the sender from configuration.
func New(cfg config.Mail) *SMTP {
	return &SMTP{sender: mail.Sender{
		Host: cfg.Host,
		Port: cfg.Port,
		User: cfg.User,
		Pass: cfg.Pass,
		From: cfg.From,
		Name: cfg.FromName,
	}}
}

// Send delivers a plain text message.
func (s *SMTP) Send(to []string, subject, body string) error {
	return s.sender.Send(to, subject, body)
}
