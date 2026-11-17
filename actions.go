package main

// Outgoing mail, shared by the web UI and the MCP endpoint. Every message that
// leaves is kept in the sent bucket for the same TTL as received mail.

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

const maxComposeFiles = 10

// upload is one attachment of a new mail.
type upload struct {
	Name, Type string
	Data       []byte
}

// userErr is a problem with the input, safe to show as is.
type userErr struct{ msg string }

func (e userErr) Error() string { return e.msg }

func badInput(format string, a ...any) error { return userErr{fmt.Sprintf(format, a...)} }

// fromDomains lists the allowed domains, which are the only ones mail may be
// sent from.
func fromDomains() []string {
	var out []string
	for d := range rules.Load().domains {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// defaultFrom is FORWARD_FROM when it is at an allowed domain, else
// mailer@ the first allowed domain.
func defaultFrom() string {
	doms := rules.Load().domains
	if forwardFromAddr != "" && doms[domainOf(forwardFromAddr)] {
		return strings.ToLower(forwardFromAddr)
	}
	if list := fromDomains(); len(list) > 0 {
		return "mailer@" + list[0]
	}
	return ""
}

// checkFrom validates a sender address; it must be at an allowed domain.
func checkFrom(addr string) (string, error) {
	if strings.TrimSpace(addr) == "" {
		if d := defaultFrom(); d != "" {
			return d, nil
		}
		return "", badInput("add an allowed domain first: mail can only be sent from one")
	}
	a, err := normDest(kindEmail, addr)
	if err != nil {
		return "", badInput("From: %v", err)
	}
	if !rules.Load().domains[domainOf(a)] {
		return "", badInput("From must be at an allowed domain (%s)", strings.Join(fromDomains(), ", "))
	}
	return a, nil
}

func checkTo(addr string) (string, error) {
	a, err := normDest(kindEmail, addr)
	if err != nil {
		return "", badInput("To: %v", err)
	}
	return a, nil
}

func sentDate(t time.Time) string { return t.Format("2 Jan 2006, 15:04 MST") }

// sendNew sends a new mail and keeps it in sent. The attachments are stored
// first, since delivery streams them from the DB; a failed send is removed
// again, so sent holds only what left.
func sendNew(from, to, subject, text string, files []upload) (*Mail, error) {
	from, err := checkFrom(from)
	if err != nil {
		return nil, err
	}
	if to, err = checkTo(to); err != nil {
		return nil, err
	}
	subject = truncate(headerSafe(strings.TrimSpace(subject)), 300)
	text = strings.TrimRight(text, " \t\r\n")
	if subject == "" && text == "" && len(files) == 0 {
		return nil, badInput("write a subject or some text first")
	}
	if len(files) > maxComposeFiles {
		return nil, badInput("at most %d attachments", maxComposeFiles)
	}
	var total int64
	metas := make([]FileMeta, len(files))
	datas := make([][]byte, len(files))
	for i, f := range files {
		total += int64(len(f.Data))
		name := safeFilename(f.Name, i+1)
		metas[i] = FileMeta{Name: name, Size: int64(len(f.Data)), Type: orDefault(f.Type, "application/octet-stream"), Stored: true}
		datas[i] = f.Data
	}
	if total > storeMaxBytes {
		return nil, badInput("attachments total %s; the limit is %s", humanSize(total), humanSize(storeMaxBytes))
	}

	now := time.Now()
	m := &Mail{At: now, Kind: sentCompose, From: from, FromAddr: from, To: to, ToAddr: to,
		Subject: subject, Date: sentDate(now), MsgID: newMessageID(from), Files: metas}
	if err := db.saveRow(bSent, m, &Body{Text: text}, datas); err != nil {
		return nil, err
	}
	err = deliver(&outMsg{From: from, To: to, Subject: subject, Text: text, MsgID: m.MsgID, MailID: m.ID, Files: metas})
	if err != nil {
		log.Printf("compose %s -> %s: %v", from, to, err)
		db.deleteRow(bSent, m.ID)
		return nil, fmt.Errorf("not sent: %w", err)
	}
	db.appendRowLog(bSent, m.ID, "✉ sent to "+to)
	return m, nil
}

// sendReply answers an inbox mail, quoting it, and keeps the reply in sent.
func sendReply(m *Mail, to, subject, text string) (*Mail, error) {
	b, err := db.getBody(m.ID)
	if err != nil {
		return nil, errNotFound
	}
	if strings.TrimSpace(to) == "" {
		to = replyAddress(m)
	}
	if to, err = checkTo(to); err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, badInput("write something first")
	}
	subject = orDefault(strings.TrimSpace(subject), prefixed("Re:", m.Subject))
	out := composeReply(m, b, to, subject, text)
	out.MsgID = newMessageID(out.From)
	if err := deliver(out); err != nil {
		log.Printf("reply %s -> %s: %v", idHex(m.ID), to, err)
		db.appendLog(m.ID, "↩ reply to "+to+": failed: "+truncate(err.Error(), 200))
		return nil, fmt.Errorf("not sent: %w", err)
	}
	db.updateMail(m.ID, func(x *Mail) { x.Replied = true })
	db.appendLog(m.ID, "↩ replied to "+to)

	now := time.Now()
	s := &Mail{At: now, Kind: sentReply, Orig: m.ID, From: out.From, FromAddr: out.From, To: to, ToAddr: to,
		Subject: out.Subject, Date: sentDate(now), MsgID: out.MsgID, Refs: out.Refs}
	if err := db.saveRow(bSent, s, &Body{Text: out.Text}, nil); err != nil {
		log.Printf("reply %s: keep in sent: %v", idHex(m.ID), err)
		return s, nil // it went out; only the copy is missing
	}
	db.appendRowLog(bSent, s.ID, "↩ sent to "+to)
	return s, nil
}

// queueForward forwards an inbox mail in the background and records it in
// sent, whose log then follows the delivery attempts. Its attachments stay
// with the original and are not copied.
func queueForward(m *Mail, to string) (*Mail, error) {
	to, err := checkTo(to)
	if err != nil {
		return nil, err
	}
	b, err := db.getBody(m.ID)
	if err != nil {
		return nil, errNotFound
	}
	out := composeForward(m, b, to)
	files := make([]FileMeta, len(m.Files))
	for i, f := range m.Files {
		f.Stored = false
		files[i] = f
	}
	now := time.Now()
	s := &Mail{At: now, Kind: sentForward, Orig: m.ID, From: out.From, FromAddr: out.From, To: to, ToAddr: to,
		Subject: out.Subject, Date: sentDate(now), Files: files}
	var sentID uint64
	if err := db.saveRow(bSent, s, &Body{Text: out.Text}, nil); err != nil {
		log.Printf("forward %s: keep in sent: %v", idHex(m.ID), err)
	} else {
		sentID = s.ID
	}
	j := fwdJob{mailID: m.ID, sentID: sentID, to: to}
	j.log("✉ " + to + ": queued")
	enqueueForward(j)
	return s, nil
}

// userMessage is err as shown to a user: input problems verbatim, a missing
// mail as such, and anything else with its text.
func userMessage(err error) string {
	if errors.Is(err, errNotFound) {
		return "mail not found (deleted or expired)"
	}
	return err.Error()
}
