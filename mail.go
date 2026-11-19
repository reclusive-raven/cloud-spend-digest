package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"
)

// smtpConfig comes from the environment, so credentials stay out of argv
// and shell history.
type smtpConfig struct {
	Host, Port, Username, Password, From string
}

func smtpFromEnv() (smtpConfig, error) {
	c := smtpConfig{
		Host:     os.Getenv("SMTP_HOST"),
		Port:     os.Getenv("SMTP_PORT"),
		Username: os.Getenv("SMTP_USERNAME"),
		// Gmail shows app passwords in groups of four; the spaces are not part of it.
		Password: strings.ReplaceAll(os.Getenv("SMTP_PASSWORD"), " ", ""),
		From:     os.Getenv("SMTP_FROM"),
	}
	if c.Host == "" {
		c.Host = "smtp.gmail.com"
	}
	if c.Port == "" {
		c.Port = "465"
	}
	if c.From == "" {
		c.From = c.Username
	}
	if c.Username == "" || c.Password == "" {
		return c, errors.New("sending needs SMTP_USERNAME and SMTP_PASSWORD")
	}
	return c, nil
}

const plainText = "This is the month's cloud spend digest. It is formatted as HTML; open it in a mail client that shows HTML."

// buildMessage renders an RFC 5322 message: multipart/alternative with the
// plain part first, both quoted-printable so the wire stays 7-bit.
func buildMessage(from string, to []string, subject, html string, now time.Time) ([]byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range []struct{ ctype, content string }{{"text/plain; charset=utf-8", plainText}, {"text/html; charset=utf-8", html}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {p.ctype}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		qw := quotedprintable.NewWriter(pw)
		if _, err := qw.Write([]byte(p.content)); err != nil {
			return nil, err
		}
		if err := qw.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := "localhost"
	if _, d, ok := strings.Cut(from, "@"); ok && d != "" {
		domain = d
	}
	var out bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&out, "%s: %s\r\n", k, v) }
	header("From", from)
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// send delivers msg. Port 465 is implicit TLS; any other port must offer
// STARTTLS, so credentials never cross the wire in clear.
func send(c smtpConfig, to []string, msg []byte) error {
	addr := net.JoinHostPort(c.Host, c.Port)
	tlsConfig := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	if c.Port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer client.Close()
	if c.Port != "465" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("smtp server offers no STARTTLS; refusing to send credentials in clear")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
		return errors.New("smtp authentication failed")
	}
	if err := client.Mail(c.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT TO refused")
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp DATA not accepted: %w", err)
	}
	return client.Quit()
}
