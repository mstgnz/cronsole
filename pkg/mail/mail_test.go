package mail

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a server that speaks just enough SMTP ----------------------------------

type fakeSMTP struct {
	listener net.Listener

	// offerSTARTTLS decides whether EHLO advertises it.
	offerSTARTTLS bool
	// tlsConfig is used when STARTTLS is offered and accepted.
	tlsConfig *tls.Config
	// rejectRecipient answers RCPT with a 550.
	rejectRecipient bool
	// rejectData answers the final dot with a 554, which is a server accepting
	// the envelope and then refusing the message.
	rejectData bool

	mu       sync.Mutex
	messages []string
	envelope []string

	done sync.WaitGroup
}

func startFakeSMTP(t *testing.T, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &fakeSMTP{listener: listener}
	if configure != nil {
		configure(s)
	}

	s.done.Add(1)
	go func() {
		defer s.done.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.serve(conn)
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		s.done.Wait()
	})
	return s
}

func (s *fakeSMTP) addr() (host, port string) {
	host, port, _ = net.SplitHostPort(s.listener.Addr().String())
	return host, port
}

func (s *fakeSMTP) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func (s *fakeSMTP) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.envelope...)
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	reader := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	write("220 fake.example.com ESMTP")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))

		s.mu.Lock()
		s.envelope = append(s.envelope, strings.TrimSpace(line))
		s.mu.Unlock()

		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			if s.offerSTARTTLS {
				write("250-fake.example.com")
				write("250-STARTTLS")
				write("250 AUTH PLAIN")
				continue
			}
			write("250-fake.example.com")
			write("250 AUTH PLAIN")

		case strings.HasPrefix(command, "STARTTLS"):
			write("220 ready")
			tlsConn := tls.Server(conn, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			reader = bufio.NewReader(conn)
			write = func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

		case strings.HasPrefix(command, "AUTH"):
			write("235 accepted")

		case strings.HasPrefix(command, "MAIL FROM"):
			write("250 ok")

		case strings.HasPrefix(command, "RCPT TO"):
			if s.rejectRecipient {
				write("550 no such user")
				continue
			}
			write("250 ok")

		case strings.HasPrefix(command, "DATA"):
			write("354 go ahead")
			var body strings.Builder
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if dataLine == ".\r\n" {
					break
				}
				body.WriteString(dataLine)
			}
			if s.rejectData {
				write("554 message rejected")
				continue
			}
			s.mu.Lock()
			s.messages = append(s.messages, body.String())
			s.mu.Unlock()
			write("250 queued")

		case strings.HasPrefix(command, "QUIT"):
			write("221 bye")
			return

		default:
			write("250 ok")
		}
	}
}

// selfSigned makes a certificate no client will trust, which is exactly what is
// needed to prove that verification is not being skipped.
func selfSigned(t *testing.T) *tls.Config {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}
}

func senderFor(s *fakeSMTP) Sender {
	host, port := s.addr()
	return Sender{Host: host, Port: port, From: "cron@example.com", Name: "Cronsole"}
}

// --- configuration ----------------------------------------------------------

func TestConfigured(t *testing.T) {
	// User and Pass are deliberately NOT required: an internal relay that
	// accepts mail from inside the network without authentication is a normal
	// arrangement, and demanding credentials would rule it out.
	cases := []struct {
		sender Sender
		want   bool
	}{
		{Sender{Host: "smtp.example.com", From: "a@b.c"}, true},
		{Sender{Host: "smtp.example.com", From: "a@b.c", User: "u", Pass: "p"}, true},
		{Sender{Host: "smtp.example.com"}, false},
		{Sender{From: "a@b.c"}, false},
		{Sender{}, false},
	}
	for _, c := range cases {
		if got := c.sender.Configured(); got != c.want {
			t.Errorf("Configured(%+v) = %v, want %v", c.sender, got, c.want)
		}
	}
}

func TestSendWithNoServerConfigured(t *testing.T) {
	err := Sender{}.Send([]string{"ops@example.com"}, "subject", "body")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Send = %v, want ErrNotConfigured", err)
	}
}

func TestSendToNobodyIsNotAnError(t *testing.T) {
	// A job with no recipient list is the ordinary case, and the caller should
	// not have to check before every send.
	s := Sender{Host: "smtp.example.com", From: "a@b.c"}
	if err := s.Send(nil, "subject", "body"); err != nil {
		t.Errorf("Send to nobody = %v, want nil", err)
	}
	if err := s.Send([]string{}, "subject", "body"); err != nil {
		t.Errorf("Send to an empty list = %v, want nil", err)
	}
}

// --- header injection -------------------------------------------------------

func TestASubjectWithALineBreakIsRefused(t *testing.T) {
	// A header carrying a newline lets a second header, or a whole second
	// message, be smuggled after it. Subjects carry a job name and a job name
	// is operator supplied.
	s := Sender{Host: "smtp.example.com", From: "a@b.c"}

	for _, subject := range []string{
		"ok\r\nBcc: attacker@example.com",
		"ok\nBcc: attacker@example.com",
		"ok\r",
	} {
		err := s.Send([]string{"ops@example.com"}, subject, "body")
		if err == nil {
			t.Errorf("a subject containing a line break was accepted: %q", subject)
		}
	}
}

func TestAnAddressThatIsNotOneIsRefused(t *testing.T) {
	s := Sender{Host: "smtp.example.com", From: "a@b.c"}

	for _, address := range []string{
		"ops@example.com\r\nRCPT TO: <attacker@example.com>",
		"ops@example.com attacker@example.com",
		"not-an-address",
		"",
	} {
		if err := s.Send([]string{address}, "subject", "body"); err == nil {
			t.Errorf("%q was accepted as a recipient", address)
		}
	}
}

// --- delivery ---------------------------------------------------------------

func TestSendDeliversTheMessage(t *testing.T) {
	server := startFakeSMTP(t, nil)
	sender := senderFor(server)

	err := sender.Send([]string{"ops@example.com", "oncall@example.com"},
		"daily-report failed", "HTTP 500 from https://shop.example.com/cron/daily")
	if err != nil {
		t.Fatalf("Send = %v", err)
	}

	messages := server.delivered()
	if len(messages) != 1 {
		t.Fatalf("%d messages were delivered, want 1", len(messages))
	}

	message := messages[0]
	for _, want := range []string{
		"From: Cronsole <cron@example.com>",
		"To: ops@example.com, oncall@example.com",
		"Subject: daily-report failed",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		// Alerts become noise the moment somebody replies to them or files them
		// as a conversation.
		"Auto-Submitted: auto-generated",
		"HTTP 500 from https://shop.example.com/cron/daily",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the message is missing %q:\n%s", want, message)
		}
	}

	// Both recipients reached the envelope, not just the To header. A header is
	// display; the envelope is delivery.
	commands := strings.Join(server.commands(), "\n")
	for _, want := range []string{"ops@example.com", "oncall@example.com"} {
		if !strings.Contains(commands, "RCPT TO:<"+want+">") {
			t.Errorf("%s is not in the envelope:\n%s", want, commands)
		}
	}
}

func TestARefusedRecipientIsReported(t *testing.T) {
	server := startFakeSMTP(t, func(s *fakeSMTP) { s.rejectRecipient = true })

	err := senderFor(server).Send([]string{"nobody@example.com"}, "subject", "body")
	if err == nil {
		t.Fatal("a refused recipient was reported as success")
	}
	if !strings.Contains(err.Error(), "nobody@example.com") {
		t.Errorf("err = %v, want it to name the recipient", err)
	}
}

func TestAMessageRefusedAtTheFinalDotIsReported(t *testing.T) {
	// The close is what commits the message, so its error is the one that says
	// whether it was accepted. Deferring it would swallow exactly this case and
	// report a message that was never queued as sent.
	server := startFakeSMTP(t, func(s *fakeSMTP) { s.rejectData = true })

	if err := senderFor(server).Send([]string{"ops@example.com"}, "subject", "body"); err == nil {
		t.Fatal("a message the server rejected was reported as sent")
	}
}

func TestAnUnreachableServerIsReported(t *testing.T) {
	// A closed port, so the dial fails immediately rather than on the timeout.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()

	sender := Sender{Host: host, Port: port, From: "cron@example.com"}
	if err := sender.Send([]string{"ops@example.com"}, "subject", "body"); err == nil {
		t.Fatal("sending to a closed port was reported as success")
	}
}

// --- transport security -----------------------------------------------------

func TestCredentialsAreRefusedWithoutSTARTTLS(t *testing.T) {
	// Sending a password in clear text is not a degraded mode to fall back
	// into, it is a disclosure. A server that offers no STARTTLS and wants
	// credentials is refused rather than accommodated.
	server := startFakeSMTP(t, nil) // no STARTTLS advertised

	sender := senderFor(server)
	sender.User, sender.Pass = "cron", "hunter2"

	err := sender.Send([]string{"ops@example.com"}, "subject", "body")
	if err == nil {
		t.Fatal("the password would have been sent in clear text")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("err = %v, want it to explain that STARTTLS is missing", err)
	}

	// And nothing was sent, so the credentials never reached the wire.
	commands := strings.Join(server.commands(), "\n")
	if strings.Contains(strings.ToUpper(commands), "AUTH") {
		t.Errorf("an AUTH command was issued over a clear connection:\n%s", commands)
	}
}

func TestNoCredentialsMeansNoSTARTTLSIsRequired(t *testing.T) {
	// The internal relay case: no password to leak, so a plain connection is a
	// deployment decision rather than a disclosure.
	server := startFakeSMTP(t, nil)

	if err := senderFor(server).Send([]string{"ops@example.com"}, "subject", "body"); err != nil {
		t.Fatalf("Send = %v", err)
	}
	if len(server.delivered()) != 1 {
		t.Error("the message was not delivered")
	}
}

func TestCertificateVerificationIsNotSkipped(t *testing.T) {
	// The fault this package was written to replace: the previous client
	// disabled verification, which hands the SMTP password to anyone in the
	// path. A self-signed certificate has to fail.
	server := startFakeSMTP(t, func(s *fakeSMTP) {
		s.offerSTARTTLS = true
		s.tlsConfig = selfSigned(t)
	})

	err := senderFor(server).Send([]string{"ops@example.com"}, "subject", "body")
	if err == nil {
		t.Fatal("an untrusted certificate was accepted, so verification is off")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("err = %v, want the STARTTLS failure", err)
	}
}

func TestPort465IsImplicitTLS(t *testing.T) {
	// 465 is encrypted before anything is said; everything else starts in clear
	// and upgrades. Treating them the same is why the previous client could not
	// reach port 587 at all, so the two paths have to stay distinguishable.
	//
	// The server here speaks TLS immediately with an untrusted certificate. A
	// client that dialled in clear would fail with a protocol error; one that
	// dialled TLS fails with a certificate error. The message says which.
	listener, err := tls.Listen("tcp", "127.0.0.1:0", selfSigned(t))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("220 fake ESMTP\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	host, _, _ := net.SplitHostPort(listener.Addr().String())
	// The port has to be the literal 465 for the implicit branch to be taken,
	// so this dials a port nothing is listening on and asserts on the wording,
	// which is the only part that differs between the two branches.
	sender := Sender{Host: host, Port: "465", From: "cron@example.com"}

	err = sender.Send([]string{"ops@example.com"}, "subject", "body")
	if err == nil {
		t.Fatal("Send succeeded against a port nothing is listening on")
	}
	if !strings.Contains(err.Error(), "TLS connection") {
		t.Errorf("err = %v, want the implicit TLS branch to have been taken", err)
	}
}

// --- the message itself -----------------------------------------------------

func TestComposeEscapesALeadingDot(t *testing.T) {
	// A line that is only a dot ends the message early in SMTP, and a job's
	// output can contain anything. Without the escape, everything after such a
	// line is silently lost, or worse, interpreted as SMTP commands.
	s := Sender{From: "cron@example.com"}
	message := s.compose([]string{"ops@example.com"}, "subject",
		"first line\n.\nafter the dot\n.hidden")

	if strings.Contains(message, "\r\n.\r\n") {
		t.Errorf("a bare dot line survived, ending the message early:\n%q", message)
	}
	if !strings.Contains(message, "\r\n..\r\n") {
		t.Errorf("the bare dot was not escaped:\n%q", message)
	}
	if !strings.Contains(message, "\r\n..hidden\r\n") {
		t.Errorf("a leading dot was not escaped:\n%q", message)
	}
	if !strings.Contains(message, "after the dot") {
		t.Error("the body after the dot was lost")
	}
}

func TestComposeNormalisesLineEndings(t *testing.T) {
	// SMTP wants CRLF. A body arriving with CRLF already must not become CRCRLF,
	// which some servers reject and others render as a blank line per line.
	s := Sender{From: "cron@example.com"}
	message := s.compose([]string{"ops@example.com"}, "subject", "one\r\ntwo\nthree")

	if strings.Contains(message, "\r\r\n") {
		t.Errorf("line endings were doubled:\n%q", message)
	}
	for _, want := range []string{"one\r\n", "two\r\n", "three\r\n"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message is missing %q:\n%q", want, message)
		}
	}
}

func TestComposeFromHeader(t *testing.T) {
	withName := Sender{From: "cron@example.com", Name: "Cronsole"}
	if got := withName.compose(nil, "s", "b"); !strings.Contains(got, "From: Cronsole <cron@example.com>") {
		t.Errorf("From header = %q", firstLine(got))
	}

	// And a bare address when there is no name, rather than "<addr>" with an
	// empty display name, which some servers treat as malformed.
	withoutName := Sender{From: "cron@example.com"}
	if got := withoutName.compose(nil, "s", "b"); !strings.Contains(got, "From: cron@example.com\r\n") {
		t.Errorf("From header = %q", firstLine(got))
	}
}

func TestComposeSeparatesHeadersFromTheBody(t *testing.T) {
	// One blank line, or the body is read as more headers.
	s := Sender{From: "cron@example.com"}
	message := s.compose([]string{"ops@example.com"}, "subject", "the body")

	headers, body, found := strings.Cut(message, "\r\n\r\n")
	if !found {
		t.Fatalf("there is no header separator:\n%q", message)
	}
	if !strings.Contains(headers, "Subject: subject") {
		t.Errorf("the subject is not in the headers:\n%q", headers)
	}
	if !strings.HasPrefix(body, "the body") {
		t.Errorf("the body is %q", body)
	}
}

func TestComposeCarriesADate(t *testing.T) {
	// Without one, several servers add their own on receipt and a message
	// delayed in a queue appears to have been sent when it was delivered.
	s := Sender{From: "cron@example.com"}
	message := s.compose(nil, "s", "b")

	line := ""
	for _, l := range strings.Split(message, "\r\n") {
		if strings.HasPrefix(l, "Date: ") {
			line = strings.TrimPrefix(l, "Date: ")
		}
	}
	if line == "" {
		t.Fatal("there is no Date header")
	}
	if _, err := time.Parse(time.RFC1123Z, line); err != nil {
		t.Errorf("Date %q is not RFC1123Z: %v", line, err)
	}
}

func TestValidAddresses(t *testing.T) {
	if err := validAddresses([]string{"a@b.c", "ops@example.com"}); err != nil {
		t.Errorf("validAddresses = %v", err)
	}
	for _, address := range []string{"a b@c.d", "a@b.c\n", "a@b.c\r", "no-at-sign"} {
		if err := validAddresses([]string{address}); err == nil {
			t.Errorf("%q was accepted", address)
		}
	}
}

func TestPortDefaultsTo587(t *testing.T) {
	// 587 is submission with STARTTLS, which is what nearly every provider
	// wants. Defaulting to 25 would reach a relay that refuses to submit.
	server := startFakeSMTP(t, nil)
	host, port := server.addr()

	sender := Sender{Host: host, Port: "", From: "cron@example.com"}
	// The fake server is not on 587, so this must fail against 587 rather than
	// silently succeed against some other default.
	err := sender.Send([]string{"ops@example.com"}, "subject", "body")
	if err == nil {
		t.Fatal("an empty port reached a server")
	}
	if !strings.Contains(err.Error(), ":587") {
		t.Errorf("err = %v, want the attempt to have been on port 587", err)
	}
	_ = port
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\r\n")
	return line
}
