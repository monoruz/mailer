package main

// Outbound mail for replies and email destinations, delivered straight to the
// recipient's MX on port 25. Mail is not DKIM-signed; publish an SPF record
// that names this host, or expect it to land in spam. Many cloud providers
// block outbound port 25 by default.

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"sort"
	"strings"
	"time"
)

// outMsg is one mail to send. Attachments are read from the stored mail
// MailID one at a time while writing, so at most one is held in memory.
type outMsg struct {
	From, To, Subject string
	ReplyTo           string
	InReplyTo, Refs   string
	MsgID             string // generated when empty
	Text, HTML        string
	MailID            uint64
	Files             []FileMeta // index-aligned with the stored mail's Files
}

var heloName string // MAIL_HOSTNAME, else the machine's hostname

// deliver tries each MX of the recipient's domain in preference order.
func deliver(m *outMsg) error {
	domain := domainOf(m.To)
	if domain == "" {
		return permanent(errors.New("recipient has no domain"))
	}
	var hosts []string
	mxs, err := net.LookupMX(domain)
	switch {
	case err == nil && len(mxs) == 1 && mxs[0].Host == ".":
		return permanent(fmt.Errorf("%s accepts no mail (null MX)", domain))
	case err == nil && len(mxs) > 0:
		sort.Slice(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
		for _, mx := range mxs {
			hosts = append(hosts, strings.TrimSuffix(mx.Host, "."))
		}
	default:
		hosts = []string{domain} // RFC 5321 implicit MX
	}
	var last error
	for _, h := range hosts {
		last = sendVia(h, m)
		if last == nil || isPermanent(last) {
			return last
		}
		debugf("deliver %s via %s: %v", m.To, h, last)
	}
	return last
}

func sendVia(host string, m *outMsg) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "25"), 20*time.Second)
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(5 * time.Minute))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello(heloName); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		// Opportunistic TLS, as MTAs do by default: encrypts against passive
		// listeners. MX certificates are too often self-signed to require.
		if err := c.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if err := c.Mail(m.From); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if err := writeMIME(w, m); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	c.Quit()
	return nil
}

type permErr struct{ error }

func permanent(err error) error { return permErr{err} }

// isPermanent reports a 5xx reply or a failure retrying cannot fix.
func isPermanent(err error) bool {
	var p permErr
	if errors.As(err, &p) {
		return true
	}
	var t *textproto.Error
	return errors.As(err, &t) && t.Code >= 500
}

// ------------------------------------------------------------------ MIME

func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return ' '
		}
		return r
	}, s)
}

func encodeWord(s string) string { return mime.QEncoding.Encode("utf-8", headerSafe(s)) }

func newMessageID(from string) string {
	b := make([]byte, 12)
	rand.Read(b)
	d := domainOf(from)
	if d == "" {
		d = heloName
	}
	return "<" + hex.EncodeToString(b) + "@" + d + ">"
}

// writeMIME streams the message: multipart/mixed holding the text (or a
// text+HTML alternative) followed by the attachments, all 7-bit safe.
func writeMIME(w io.Writer, m *outMsg) error {
	h := &strings.Builder{}
	hdr := func(k, v string) {
		if v != "" {
			fmt.Fprintf(h, "%s: %s\r\n", k, v)
		}
	}
	hdr("From", (&mail.Address{Address: m.From}).String())
	hdr("To", (&mail.Address{Address: m.To}).String())
	if m.ReplyTo != "" {
		hdr("Reply-To", (&mail.Address{Address: m.ReplyTo}).String())
	}
	hdr("Subject", encodeWord(m.Subject))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", orDefault(headerSafe(m.MsgID), newMessageID(m.From)))
	hdr("In-Reply-To", headerSafe(m.InReplyTo))
	hdr("References", headerSafe(m.Refs))
	hdr("MIME-Version", "1.0")

	mw := multipart.NewWriter(w)
	hdr("Content-Type", `multipart/mixed; boundary="`+mw.Boundary()+`"`)
	if _, err := io.WriteString(w, h.String()+"\r\n"); err != nil {
		return err
	}

	if m.HTML == "" {
		if err := textPart(mw, "text/plain", m.Text); err != nil {
			return err
		}
	} else {
		alt := multipart.NewWriter(io.Discard) // only for a boundary
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type": {`multipart/alternative; boundary="` + alt.Boundary() + `"`},
		})
		if err != nil {
			return err
		}
		aw := multipart.NewWriter(pw)
		aw.SetBoundary(alt.Boundary())
		if err := textPart(aw, "text/plain", m.Text); err != nil {
			return err
		}
		if err := textPart(aw, "text/html", m.HTML); err != nil {
			return err
		}
		if err := aw.Close(); err != nil {
			return err
		}
	}

	for i, f := range m.Files {
		if !f.Stored {
			continue
		}
		data, err := db.getAttachment(m.MailID, i)
		if err != nil {
			continue // expired or deleted meanwhile
		}
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mime.FormatMediaType(orDefault(f.Type, "application/octet-stream"), map[string]string{"name": f.Name})},
			"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": f.Name})},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return err
		}
		enc := base64.NewEncoder(base64.StdEncoding, &lineWrapper{w: pw})
		if _, err := enc.Write(data); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		data = nil
	}
	return mw.Close()
}

func textPart(mw *multipart.Writer, ctype, body string) error {
	pw, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {ctype + "; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return err
	}
	qw := quotedprintable.NewWriter(pw)
	if _, err := io.WriteString(qw, body); err != nil {
		return err
	}
	return qw.Close()
}

// lineWrapper breaks base64 output into 76-column lines.
type lineWrapper struct {
	w   io.Writer
	col int
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := min(76-l.col, len(p))
		if _, err := l.w.Write(p[:chunk]); err != nil {
			return n, err
		}
		n += chunk
		l.col += chunk
		p = p[chunk:]
		if l.col == 76 {
			if _, err := io.WriteString(l.w, "\r\n"); err != nil {
				return n, err
			}
			l.col = 0
		}
	}
	return n, nil
}

// -------------------------------------------------------------- compose

// plainBody is the mail's text, or its HTML flattened when there is none.
func plainBody(b *Body) string {
	if t := tidyText(b.Text, false); t != "" {
		return t
	}
	return htmlToText(b.HTML)
}

func prefixed(prefix, subject string) string {
	if strings.HasPrefix(strings.ToLower(subject), strings.ToLower(prefix)) {
		return subject
	}
	return prefix + " " + subject
}

// replyAddress is where a reply goes: Reply-To when set, else From.
func replyAddress(m *Mail) string { return orDefault(m.ReplyTo, m.FromAddr) }

// composeReply quotes the original under the new text and threads it.
func composeReply(m *Mail, b *Body, to, subject, text string) *outMsg {
	var q strings.Builder
	q.WriteString(strings.TrimRight(text, "\r\n \t"))
	fmt.Fprintf(&q, "\n\nOn %s, %s wrote:\n", m.Date, m.From)
	quoted := truncate(plainBody(b), 20000)
	for _, line := range strings.Split(quoted, "\n") {
		q.WriteString("> " + line + "\n")
	}
	refs := strings.TrimSpace(m.Refs + " " + m.MsgID)
	return &outMsg{
		From:      m.ToAddr,
		To:        to,
		Subject:   headerSafe(subject),
		InReplyTo: m.MsgID,
		Refs:      refs,
		Text:      q.String(),
	}
}

// composeForward wraps the original, with its stored attachments.
func composeForward(m *Mail, b *Body, to string) *outMsg {
	head := [][2]string{{"From", m.From}, {"Date", m.Date}, {"Subject", m.Subject}, {"To", m.To}, {"Cc", m.Cc}}
	var t, h strings.Builder
	t.WriteString("---------- Forwarded message ----------\n")
	h.WriteString(`<div style="font:13px sans-serif;color:#555;border-bottom:1px solid #ddd;padding-bottom:8px;margin-bottom:12px">---------- Forwarded message ----------<br>`)
	for _, kv := range head {
		if kv[1] != "" {
			t.WriteString(kv[0] + ": " + kv[1] + "\n")
			h.WriteString("<b>" + kv[0] + ":</b> " + escape(kv[1]) + "<br>")
		}
	}
	var missing []string
	for _, f := range m.Files {
		if !f.Stored {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		note := "Not attached (too large to keep): " + strings.Join(missing, ", ")
		t.WriteString(note + "\n")
		h.WriteString("<i>" + escape(note) + "</i><br>")
	}
	t.WriteString("\n" + plainBody(b))
	h.WriteString("</div>")

	out := &outMsg{
		From:    forwardFrom(m),
		To:      to,
		ReplyTo: m.FromAddr,
		Subject: prefixed("Fwd:", m.Subject),
		Text:    t.String(),
		MailID:  m.ID,
		Files:   m.Files,
	}
	if b.HTML != "" {
		clean, _ := sanitizeHTML(b.HTML)
		out.HTML = h.String() + clean
	}
	return out
}

var forwardFromAddr string // FORWARD_FROM; empty: the address the mail was sent to

func forwardFrom(m *Mail) string { return orDefault(forwardFromAddr, m.ToAddr) }

// ---------------------------------------------------------------- queue

// Email destinations are delivered in the background with backoff, so a slow
// or greylisting MX never holds up the SMTP session. Pending retries live in
// memory only and are lost on restart.
type fwdJob struct {
	mailID uint64
	sentID uint64 // its record in the sent bucket; 0 for none
	to     string
	try    int
}

// log records an outcome on the original mail and on its sent record.
func (j fwdJob) log(line string) {
	db.appendLog(j.mailID, line)
	if j.sentID != 0 {
		db.appendRowLog(bSent, j.sentID, line)
	}
}

var (
	fwdQueue  = make(chan fwdJob, 256)
	fwdDelays = []time.Duration{2 * time.Minute, 10 * time.Minute, 45 * time.Minute, 3 * time.Hour}
)

func enqueueForward(j fwdJob) {
	select {
	case fwdQueue <- j:
	default:
		log.Printf("forward: queue full, dropping %s -> %s", idHex(j.mailID), j.to)
		j.log("✉ " + j.to + ": dropped, queue full")
	}
}

func forwardWorker() {
	for j := range fwdQueue {
		m, err := db.getMail(j.mailID)
		if err != nil {
			continue // expired or deleted
		}
		b, err := db.getBody(j.mailID)
		if err != nil {
			continue
		}
		err = deliver(composeForward(m, b, j.to))
		switch {
		case err == nil:
			log.Printf("forward: %s -> %s ok", idHex(j.mailID), j.to)
			j.log("✉ " + j.to + ": forwarded")
		case isPermanent(err) || j.try >= len(fwdDelays):
			log.Printf("forward: %s -> %s failed: %v", idHex(j.mailID), j.to, err)
			j.log("✉ " + j.to + ": failed: " + truncate(err.Error(), 200))
		default:
			d := fwdDelays[j.try]
			log.Printf("forward: %s -> %s: %v (retry in %v)", idHex(j.mailID), j.to, err, d)
			j.log(fmt.Sprintf("✉ %s: %s (retry in %v)", j.to, truncate(err.Error(), 160), d))
			j.try++
			time.AfterFunc(d, func() { enqueueForward(j) })
		}
	}
}
