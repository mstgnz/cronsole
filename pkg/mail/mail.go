// Package mail sends plain text messages over SMTP.
//
// Small on purpose: this service sends two kinds of message, a run result and
// a watchdog alert, and both are plain text. Anything richer belongs to
// whatever the operator already uses for mail.
//
// It replaces an earlier client that could not have delivered a message with
// the configuration this repository documents. That one dialled implicit TLS
// unconditionally, which fails against port 587, and it disabled certificate
// verification, which hands the SMTP password to anyone in the path. Both are
// the sort of fault that stays invisible until the day the alert matters.
package mail

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// dialTimeout bounds the connection. An alert that hangs is an alert that did
// not arrive, and the caller is a background sender that must not be pinned.
const dialTimeout = 15 * time.Second

// Sender holds the server settings.
type Sender struct {
	Host string
	Port string
	User string
	Pass string
	From string
	Name string
}

// ErrNotConfigured is returned when there is no server to talk to.
var ErrNotConfigured = errors.New("mail: no server configured")

// Configured reports whether a send can be attempted.
//
// User and Pass are NOT required: an internal relay that accepts mail from
// inside the network without authentication is a normal arrangement, and
// demanding credentials would rule it out.
func (s Sender) Configured() bool {
	return s.Host != "" && s.From != ""
}

// Send delivers a plain text message.
func (s Sender) Send(to []string, subject, body string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if len(to) == 0 {
		return nil
	}
	if err := validAddresses(to); err != nil {
		return err
	}
	// A header carrying a newline lets a second header, or a whole second
	// message, be smuggled after it.
	if strings.ContainsAny(subject, "\r\n") {
		return errors.New("mail: subject contains a line break")
	}

	port := s.Port
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(s.Host, port)

	client, err := s.connect(addr, port)
	if err != nil {
		return err
	}
	defer func() { _ = client.Quit() }()

	if s.User != "" {
		// PlainAuth refuses to send credentials over an unencrypted
		// connection unless the host is localhost, which is the behaviour we
		// want: by this point the connection is either TLS or deliberately
		// local.
		if err := client.Auth(smtp.PlainAuth("", s.User, s.Pass, s.Host)); err != nil {
			return fmt.Errorf("mail: authentication failed: %w", err)
		}
	}

	if err := client.Mail(s.From); err != nil {
		return fmt.Errorf("mail: sender refused: %w", err)
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("mail: recipient %q refused: %w", recipient, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: server refused the message: %w", err)
	}
	if _, err := writer.Write([]byte(s.compose(to, subject, body))); err != nil {
		_ = writer.Close()
		return fmt.Errorf("mail: writing the message failed: %w", err)
	}
	// Closed explicitly rather than deferred: the close is what commits the
	// message, so its error is the one that says whether it was accepted.
	if err := writer.Close(); err != nil {
		return fmt.Errorf("mail: the message was not accepted: %w", err)
	}
	return nil
}

// connect opens a session, choosing between implicit TLS and STARTTLS.
//
// Port 465 is implicit TLS: the connection is encrypted before anything is
// said. Everything else, 587 and 25 included, starts in clear and upgrades
// with STARTTLS. Treating them the same is why the previous client could not
// reach the port this repository documents as the default.
func (s Sender) connect(addr, port string) (*smtp.Client, error) {
	// Certificate verification stays ON. Disabling it, as the previous client
	// did, means anyone able to intercept the connection collects the SMTP
	// password on the next send.
	tlsConfig := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: dialTimeout}

	if port == "465" {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("mail: TLS connection to %s failed: %w", addr, err)
		}
		client, err := smtp.NewClient(conn, s.Host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("mail: SMTP handshake failed: %w", err)
		}
		return client, nil
	}

	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mail: connection to %s failed: %w", addr, err)
	}
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mail: SMTP handshake failed: %w", err)
	}

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsConfig); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("mail: STARTTLS failed: %w", err)
		}
	} else if s.User != "" {
		// Sending a password in clear text is not a degraded mode to fall back
		// into, it is a disclosure. A server that offers no STARTTLS and wants
		// credentials is refused.
		_ = client.Close()
		return nil, fmt.Errorf("mail: %s does not offer STARTTLS and credentials were configured", addr)
	}
	return client, nil
}

// compose builds the message. Plain text, UTF-8, one part.
func (s Sender) compose(to []string, subject, body string) string {
	var b strings.Builder

	from := s.From
	if s.Name != "" {
		from = fmt.Sprintf("%s <%s>", s.Name, s.From)
	}
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	// Alerts are noise the moment somebody replies to them or files them as a
	// conversation, so they are marked as automatic.
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")

	// A line that is only a dot ends the message early in SMTP. Escaping it is
	// the protocol's own rule, and a job's output can contain anything.
	for line := range strings.SplitSeq(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			b.WriteString(".")
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return b.String()
}

// validAddresses rejects anything that cannot be an address, mostly to catch a
// line break before it reaches the envelope.
func validAddresses(addresses []string) error {
	for _, address := range addresses {
		if strings.ContainsAny(address, "\r\n ") || !strings.Contains(address, "@") {
			return fmt.Errorf("mail: %q is not a usable address", address)
		}
	}
	return nil
}
