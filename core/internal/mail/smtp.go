package mail

import (
	"errors"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// loginAuth is AUTH LOGIN, for servers that offer no PLAIN.
type loginAuth struct{ user, password string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.password), nil
	}
	return nil, errors.New("unexpected challenge from the mail server")
}

// recipients parses the To value ("a@x, B <b@y>") into the addresses for RCPT TO; the second
// result is false if there is none.
func recipients(to string) ([]string, bool) {
	if strings.Index(to, "@") < 1 {
		return nil, false
	}
	list, err := mail.ParseAddressList(to)
	if err != nil {
		if single := strings.TrimSpace(to); !strings.ContainsAny(single, " <>,;\r\n") {
			return []string{single}, true
		}
		return nil, false
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out, len(out) > 0
}

// smtpSend delivers one mail over SMTPS (implicit TLS, port 465 on the M5) and authenticates
// with the mailbox. The error text is the server's.
func (s *Service) smtpSend(rcpts []string, msg []byte) (err error) {
	conn, err := dialTLS(s.smtpHost, s.smtpPort)
	if err != nil {
		return loginError{err}
	}
	defer conn.Close()
	// Time for the upload: an attachment can be hundreds of KB over a slow link.
	conn.SetDeadline(time.Now().Add(45*time.Second + time.Duration(len(msg)/(16<<10))*time.Second))
	c, err := smtp.NewClient(conn, s.smtpHost)
	if err != nil {
		return loginError{err}
	}
	defer c.Close()
	if ok, params := c.Extension("AUTH"); ok {
		var auth smtp.Auth = smtp.PlainAuth("", s.address, s.password, s.smtpHost)
		if !strings.Contains(strings.ToUpper(params), "PLAIN") && strings.Contains(strings.ToUpper(params), "LOGIN") {
			auth = loginAuth{s.address, s.password}
		}
		if err := c.Auth(auth); err != nil {
			return loginError{err}
		}
	}
	if err := c.Mail(s.address); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// loginError marks a failed authentication, which the tools report differently from a failed send.
type loginError struct{ error }
