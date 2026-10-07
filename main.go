// Minimal SMTP -> Telegram relay. Standard library only.
//
// Configuration is read from a .env file when one exists (looked for at
// $ENV_FILE, then ./.env, then next to the binary); any key it does not define
// falls back to the process environment.
//
// Keys:
//
//	RADAR_BOT_TOKEN       Telegram bot token
//	TELEGRAM_CHAT_ID      Telegram chat id
//	SMTP_PORT             listen port (default 25)
//	VALID_DOMAINS         comma separated recipient domain whitelist
//	SEND_ATTACHMENTS      forward attachments as documents (default 1, "0" to disable)
//	MAX_ATTACHMENT_BYTES  per-file cap (default 20971520; Telegram bot ceiling is ~50 MB)
//	TELEGRAM_API_BASE     API base override (default https://api.telegram.org)
//	TZ                    time zone for the date line (default: the system's)
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSessionBytes  = 40 << 20 // hard cap on bytes read per connection
	maxCmdLine       = 1024
	maxTextCapture   = 256 << 10 // bytes of text/plain kept while streaming
	maxHTMLCapture   = 512 << 10 // bytes of text/html kept, for the full-email file
	maxPreviewChars  = 3000      // body shown inline; longer mail also gets an .html file
	maxTelegramText  = 4000      // UTF-16 units after entity parsing; the API limit is 4096
	maxCaptionText   = 1000      // the API limit is 1024
	maxConns         = 64
	maxMIMEDepth     = 8
	memSpoolMax      = 512 << 10 // attachments larger than this spill to a temp file
	maxAttachments   = 10        // forwarded per message; the rest are only counted
	maxSpoolTotal    = 30 << 20  // total attachment bytes spooled per message
	spoolConcurrency = 8         // messages that may hold spooled attachments at once
)

var (
	botToken        string
	chatID          string
	validDomains    = map[string]struct{}{}
	hostname        = "mailrelay"
	apiBase         = "https://api.telegram.org"
	sendAttachments = true
	debugOn         = false
	maxAttachBytes  = int64(20 << 20)
	spoolSem        = make(chan struct{}, spoolConcurrency)
	dotenv          map[string]string
	textClient      = &http.Client{Timeout: 20 * time.Second}
	uploadClient    = &http.Client{Timeout: 5 * time.Minute}
)

func main() {
	debug.SetGCPercent(40) // trade a little CPU for a smaller heap
	loadDotenv()

	botToken = getenv("RADAR_BOT_TOKEN")
	chatID = getenv("TELEGRAM_CHAT_ID")
	if v := getenv("DEBUG"); v == "1" || strings.EqualFold(v, "true") {
		debugOn = true
	}
	if botToken == "" || chatID == "" {
		log.Println("warning: RADAR_BOT_TOKEN or TELEGRAM_CHAT_ID is empty; sends will fail")
	}
	for _, d := range strings.Split(getenv("VALID_DOMAINS"), ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			validDomains[d] = struct{}{}
		}
	}
	if v := getenv("SEND_ATTACHMENTS"); v == "0" || strings.EqualFold(v, "false") {
		sendAttachments = false
	}
	if v, err := strconv.ParseInt(getenv("MAX_ATTACHMENT_BYTES"), 10, 64); err == nil && v > 0 {
		maxAttachBytes = v
	}
	if v := getenv("TELEGRAM_API_BASE"); v != "" {
		apiBase = strings.TrimSuffix(v, "/")
	}
	port := getenv("SMTP_PORT")
	if port == "" {
		port = "25"
	}
	if v := getenv("TZ"); v != "" { // also honoured from .env, unlike Go's own TZ handling
		if loc, err := time.LoadLocation(v); err == nil {
			time.Local = loc
		} else {
			log.Printf("config: TZ %q: %v", v, err)
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		hostname = h
	}

	if len(os.Args) == 3 && os.Args[1] == "-test-upload" {
		testUpload(os.Args[2])
		return
	}

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("SMTP server listening on %s", port)

	sem := make(chan struct{}, maxConns)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			serve(conn)
		}()
	}
}

// -------------------------------------------------------------- config

// getenv prefers a .env entry and falls back to the process environment.
func getenv(key string) string {
	if v, ok := dotenv[key]; ok {
		return v
	}
	return os.Getenv(key)
}

// loadDotenv reads the first .env it finds. A missing file is not an error:
// the process environment is then the only source.
func loadDotenv() {
	paths := []string{}
	if p := os.Getenv("ENV_FILE"); p != "" {
		paths = append(paths, p)
	}
	paths = append(paths, ".env")
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), ".env")) // survives a service with cwd=/
	}

	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, 64<<10))
		if fi, serr := f.Stat(); serr == nil && fi.Mode().Perm()&0o077 != 0 {
			log.Printf("config: %s is readable by other users; chmod 600 it", p)
		}
		f.Close()
		if err != nil {
			log.Printf("config: %s: %v", p, err)
			continue
		}
		dotenv = parseDotenv(b)
		log.Printf("config: loaded %d key(s) from %s", len(dotenv), p)
		return
	}
	log.Println("config: no .env found, using environment variables")
}

// parseDotenv handles KEY=VALUE, optional "export", # comments and quoting.
func parseDotenv(b []byte) map[string]string {
	m := make(map[string]string, 8)
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			quote := v[0]
			v = v[1 : len(v)-1]
			if quote == '"' {
				v = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(v)
			}
		} else if i := strings.Index(v, " #"); i >= 0 { // trailing comment
			v = strings.TrimSpace(v[:i])
		}
		m[k] = v
	}
	return m
}

// ---------------------------------------------------------------- SMTP

func serve(conn net.Conn) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil { // one bad message must not take the daemon down
			log.Printf("panic from %v: %v", conn.RemoteAddr(), r)
		}
	}()
	br := bufio.NewReaderSize(io.LimitReader(conn, maxSessionBytes), 4096)
	bw := bufio.NewWriterSize(conn, 512)
	tp := textproto.NewReader(br)

	reply := func(format string, a ...any) bool {
		fmt.Fprintf(bw, format+"\r\n", a...)
		return bw.Flush() == nil
	}

	if !reply("220 %s ESMTP ready", hostname) {
		return
	}

	var haveMail bool
	var rcpt string
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		line, err := readCmdLine(br)
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "HELO":
			reply("250 %s", hostname)
		case "EHLO":
			fmt.Fprintf(bw, "250-%s\r\n250-SIZE %d\r\n250-8BITMIME\r\n250 PIPELINING\r\n", hostname, maxSessionBytes)
			if bw.Flush() != nil {
				return
			}
		case "MAIL":
			haveMail, rcpt = true, ""
			reply("250 2.1.0 OK")
		case "RCPT":
			if !haveMail {
				reply("503 5.5.1 MAIL first")
				break
			}
			if rcpt == "" {
				rcpt = bracketed(arg)
			}
			reply("250 2.1.5 OK")
		case "DATA":
			if !haveMail || rcpt == "" {
				reply("503 5.5.1 RCPT first")
				break
			}
			if !reply("354 End data with <CR><LF>.<CR><LF>") {
				return
			}
			conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
			dot := tp.DotReader()
			err := handleMessage(dot, rcpt)
			io.Copy(io.Discard, dot) // always consume the rest of DATA
			haveMail, rcpt = false, ""
			if err != nil {
				log.Println("message:", err)
				reply("451 4.3.0 Processing error")
			} else {
				reply("250 2.0.0 OK")
			}
		case "RSET":
			haveMail, rcpt = false, ""
			reply("250 2.0.0 OK")
		case "NOOP":
			reply("250 2.0.0 OK")
		case "VRFY":
			reply("252 2.5.2 Cannot VRFY")
		case "QUIT":
			reply("221 2.0.0 Bye")
			return
		case "":
			// ignore blank lines
		default: // includes STARTTLS and AUTH: both disabled
			reply("502 5.5.1 Command not implemented")
		}
	}
}

// readCmdLine reads one CRLF-terminated command, discarding anything past maxCmdLine.
func readCmdLine(br *bufio.Reader) (string, error) {
	buf := make([]byte, 0, 64)
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return "", err
		}
		if len(buf) < maxCmdLine {
			buf = append(buf, chunk...)
		}
		if !isPrefix {
			break
		}
	}
	if len(buf) > maxCmdLine {
		buf = buf[:maxCmdLine]
	}
	return strings.TrimSpace(string(buf)), nil
}

// bracketed pulls the address out of "TO:<user@example.com>".
func bracketed(s string) string {
	if i := strings.IndexByte(s, '<'); i >= 0 {
		if j := strings.IndexByte(s[i:], '>'); j > 0 {
			return strings.TrimSpace(s[i+1 : i+j])
		}
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// ------------------------------------------------------------- message

type header interface{ Get(string) string }

type attachment struct {
	name     string
	sp       *spool // nil when the part was only counted, not spooled
	oversize bool
}

type parsed struct {
	dec        *mime.WordDecoder
	text       string
	htmlText   string
	textCut    bool // the capture limit was hit
	htmlCut    bool
	count      int
	atts       []attachment
	spooled    int64
	holdsSpool bool
}

// release frees every spooled attachment and the spool slot.
func (p *parsed) release() {
	for _, a := range p.atts {
		a.sp.close()
	}
	p.atts = nil
	if p.holdsSpool {
		<-spoolSem
		p.holdsSpool = false
	}
}

func handleMessage(r io.Reader, envelopeRcpt string) error {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return err
	}
	dec := &mime.WordDecoder{CharsetReader: charsetReader}

	rawTo := msg.Header.Get("To")
	toAddr := firstAddress(rawTo, dec)
	if toAddr == "" {
		toAddr = envelopeRcpt // header-less / Bcc-style delivery
	}
	domain := ""
	if i := strings.LastIndexByte(toAddr, '@'); i >= 0 {
		domain = strings.ToLower(toAddr[i+1:])
	}
	if _, ok := validDomains[domain]; !ok {
		log.Printf("Rejected: destination domain %q not whitelisted", domain)
		return nil
	}

	p := &parsed{dec: dec}
	defer p.release()
	p.walk(msg.Header, msg.Body, 0)

	v := &mailView{
		subject: orDefault(decodeHeader(dec, msg.Header.Get("Subject")), "(no subject)"),
		from:    orDefault(decodeHeader(dec, msg.Header.Get("From")), "Unknown sender"),
		to:      orDefault(decodeHeader(dec, rawTo), toAddr),
		cc:      decodeHeader(dec, msg.Header.Get("Cc")),
		date:    mailDate(msg.Header.Get("Date")),
		files:   p.attachmentLine(),
	}
	body, cut := tidyText(p.text, false), p.textCut
	if body == "" {
		body, cut = htmlToText(p.htmlText), p.htmlCut
	}

	summary, full := v.summary(body, cut)
	replyTo, err := sendMessage(summary)
	if err != nil {
		log.Println("sendMessage:", err)
	}

	if full {
		name, page := v.fullEmail(p)
		doc := docOpts{replyTo: replyTo, caption: "📄 Full email", silent: true}
		if err := sendDocument(name, &spool{buf: page, size: int64(len(page))}, doc); err != nil {
			log.Printf("sendDocument %s (%d bytes): %v", name, len(page), err)
		}
	}

	sent := 0
	for _, a := range p.atts {
		if a.sp == nil || a.oversize {
			continue
		}
		if err := sendDocument(a.name, a.sp, docOpts{replyTo: replyTo, silent: true}); err != nil {
			log.Printf("sendDocument %s (%d bytes): %v", a.name, a.sp.size, err)
			continue
		}
		sent++
	}
	log.Printf("mail to=%s attachments=%d forwarded=%d full=%v", toAddr, p.count, sent, full)
	return nil
}

// attachmentLine lists the attachments, or returns "" when there are none.
func (p *parsed) attachmentLine() string {
	if p.count == 0 {
		return ""
	}
	line := "1 file"
	if p.count > 1 {
		line = fmt.Sprintf("%d files", p.count)
	}
	names := make([]string, 0, len(p.atts))
	for _, a := range p.atts {
		if a.oversize {
			names = append(names, a.name+" (too large)")
		} else if a.sp == nil && sendAttachments {
			names = append(names, a.name+" (not sent)")
		} else {
			names = append(names, a.name)
		}
	}
	if len(names) > 0 {
		line += " · " + strings.Join(names, ", ")
	}
	return truncate(line, 600)
}

// walk streams one MIME entity, keeping only what the notification needs.
func (p *parsed) walk(h header, body io.Reader, depth int) {
	if depth > maxMIMEDepth {
		return
	}
	mediatype, params, err := mime.ParseMediaType(orDefault(h.Get("Content-Type"), "text/plain"))
	if err != nil {
		mediatype, params = "text/plain", nil
	}
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))

	if strings.HasPrefix(mediatype, "multipart/") && params["boundary"] != "" {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart() // skipped parts are discarded, not buffered
			if err != nil {
				return
			}
			p.walk(part.Header, part, depth+1)
			part.Close()
		}
	}

	if disp == "attachment" || dparams["filename"] != "" || params["name"] != "" ||
		!strings.HasPrefix(mediatype, "text/") {
		debugf("part depth=%d type=%s disp=%s cte=%s -> attachment", depth, mediatype, disp, h.Get("Content-Transfer-Encoding"))
		p.takeAttachment(h, body, dparams["filename"], params["name"])
		return
	}
	debugf("part depth=%d type=%s disp=%s cte=%s -> body", depth, mediatype, disp, h.Get("Content-Transfer-Encoding"))
	switch {
	case mediatype == "text/plain" && p.text == "":
		p.text, p.textCut = readText(body, h.Get("Content-Transfer-Encoding"), params["charset"], maxTextCapture)
	case mediatype == "text/html" && p.htmlText == "":
		p.htmlText, p.htmlCut = readText(body, h.Get("Content-Transfer-Encoding"), params["charset"], maxHTMLCapture)
	}
}

// takeAttachment counts a part and, when enabled and within budget, spools it for upload.
func (p *parsed) takeAttachment(h header, body io.Reader, filename, name string) {
	p.count++
	label := safeFilename(decodeHeader(p.dec, orDefault(filename, name)), p.count)

	if !sendAttachments || len(p.atts) >= maxAttachments || p.spooled >= maxSpoolTotal {
		p.atts = append(p.atts, attachment{name: label})
		return
	}
	if !p.holdsSpool {
		spoolSem <- struct{}{} // bound how much disk/memory all sessions hold at once
		p.holdsSpool = true
	}

	limit := maxAttachBytes
	if rest := maxSpoolTotal - p.spooled; rest < limit {
		limit = rest
	}
	sp, err := spoolFrom(decodeReader(body, h.Get("Content-Transfer-Encoding")), limit)
	if err != nil {
		log.Printf("spool %s: %v", label, err)
		p.atts = append(p.atts, attachment{name: label})
		return
	}
	p.spooled += sp.size
	where := "memory"
	if sp.f != nil {
		where = "tempfile"
	}
	debugf("spooled %q bytes=%d in=%s oversize=%v", label, sp.size, where, sp.oversize)
	p.atts = append(p.atts, attachment{name: label, sp: sp, oversize: sp.oversize})
}

// ---------------------------------------------------------------- spool

// spool holds one decoded attachment: in memory when small, otherwise in an
// unlinked temp file so the space is reclaimed on close (or on crash).
type spool struct {
	buf      []byte
	f        *os.File
	size     int64
	oversize bool
}

func (s *spool) close() {
	if s != nil && s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

func (s *spool) reader() (io.Reader, error) {
	if s.f != nil {
		_, err := s.f.Seek(0, io.SeekStart)
		return s.f, err
	}
	return bytes.NewReader(s.buf), nil
}

// spoolFrom is deliberately tolerant: a transfer-encoding error (unpadded
// base64 is common in the wild) truncates the attachment instead of losing it.
func spoolFrom(r io.Reader, limit int64) (*spool, error) {
	lr := io.LimitReader(r, limit+1) // +1 byte so overflow is detectable
	mem, err := io.ReadAll(io.LimitReader(lr, memSpoolMax))
	if err != nil {
		log.Printf("decode: %v (keeping %d bytes)", err, len(mem))
	}
	sp := &spool{buf: mem, size: int64(len(mem))}
	if int64(len(mem)) < memSpoolMax {
		sp.oversize = sp.size > limit
		return sp, nil
	}

	f, err := os.CreateTemp("", "mailrelay-*")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name()) // keep only the open handle: nothing is left behind
	if _, err := f.Write(mem); err != nil {
		f.Close()
		return nil, err
	}
	n, err := io.Copy(f, lr)
	if err != nil {
		log.Printf("decode: %v (keeping %d bytes)", err, int64(len(mem))+n)
	}
	sp.buf, sp.f = nil, f
	sp.size = int64(len(mem)) + n
	sp.oversize = sp.size > limit
	if sp.oversize {
		sp.close() // do not keep bytes we will not upload
	}
	return sp, nil
}

// -------------------------------------------------------------- render

// mailView holds the decoded headers shown in the chat and in the full email.
type mailView struct {
	subject, from, to, cc, date, files string
}

// tgText builds Telegram HTML while counting what the API holds against its
// length limit: UTF-16 units of the visible text, markup excluded.
type tgText struct {
	b strings.Builder
	n int
}

func (t *tgText) tag(markup string) { t.b.WriteString(markup) }

func (t *tgText) text(s string) {
	t.b.WriteString(escape(s))
	t.n += u16len(s)
}

func (t *tgText) line(icon, s string, maxChars int) {
	if s != "" {
		t.text(icon + " " + truncate(s, maxChars) + "\n")
	}
}

// summary renders the chat message, giving the body whatever room the headers
// leave. full reports that the body did not fit (or was cut while reading, per
// cut), so the whole mail should follow as a file.
func (v *mailView) summary(body string, cut bool) (msg string, full bool) {
	const footer = "\n📄 Full email attached below"
	var t tgText
	t.text("✉️ ")
	t.tag("<b>")
	t.text(truncate(v.subject, 250))
	t.tag("</b>")
	t.text("\n\n")
	t.line("👤", v.from, 200)
	t.line("📥", v.to, 300)
	t.line("👥", v.cc, 300)
	t.line("🕒", v.date, 40)

	files := ""
	if v.files != "" {
		files = "\n📎 " + v.files
	}
	room := min(maxTelegramText-t.n-u16len(files)-u16len(footer)-2, maxPreviewChars)
	preview, clipped := clip(body, room)
	full = clipped || cut

	t.text("\n")
	if preview == "" {
		t.tag("<i>")
		t.text("No text content")
		t.tag("</i>")
	} else {
		t.tag("<blockquote expandable>")
		t.text(preview)
		t.tag("</blockquote>")
	}
	t.text(files)
	if full {
		t.text(footer)
	}
	return t.b.String(), full
}

const pageHead = `<!doctype html>
<html><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'">
<meta name="referrer" content="no-referrer">
<meta name="viewport" content="width=device-width,initial-scale=1">
<base target="_blank">
<title>%s</title>
<style>
body{margin:0;padding:12px;background:#f2f2f7}
.mr-card,.mr-mail{max-width:760px;margin:0 auto 12px;border-radius:12px;box-sizing:border-box}
.mr-card{padding:16px 20px;background:#fff;color:#1c1c1e;font:15px/1.45 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;box-shadow:0 1px 3px rgba(0,0,0,.1)}
.mr-card h1{margin:0 0 12px;font-size:19px;line-height:1.3;word-break:break-word}
.mr-card dl{display:grid;grid-template-columns:auto 1fr;gap:4px 14px;margin:0}
.mr-card dt{color:#8e8e93}
.mr-card dd{margin:0;word-break:break-word}
.mr-note{margin:12px 0 0;font-size:13px;color:#8e8e93}
.mr-mail{padding:16px 20px;background:#fff;color:#1c1c1e;overflow-x:auto}
.mr-plain{margin:0;white-space:pre-wrap;word-wrap:break-word;font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}
@media (prefers-color-scheme:dark){body{background:#000}.mr-card{background:#1c1c1e;color:#f2f2f7}}
</style></head><body>
`

// fullEmail renders the whole mail as a standalone page: a header card over the
// sanitized original HTML, or over the plain text when there is no HTML part.
func (v *mailView) fullEmail(p *parsed) (name string, page []byte) {
	var b bytes.Buffer
	esc := html.EscapeString
	fmt.Fprintf(&b, pageHead, esc(v.subject))
	b.WriteString(`<div class="mr-card"><h1>` + esc(v.subject) + "</h1><dl>")
	for _, row := range [][2]string{{"From", v.from}, {"To", v.to}, {"Cc", v.cc}, {"Date", v.date}, {"Files", v.files}} {
		if row[1] != "" {
			b.WriteString("<dt>" + row[0] + "</dt><dd>" + esc(row[1]) + "</dd>")
		}
	}
	b.WriteString("</dl>")

	content, cut, blocked := "", p.textCut, 0
	if p.htmlText != "" {
		content, blocked = sanitizeHTML(p.htmlText)
		cut = p.htmlCut
	} else {
		content = `<pre class="mr-plain">` + linkify(p.text) + "</pre>"
	}
	if blocked > 0 {
		fmt.Fprintf(&b, `<p class="mr-note">%d remote image(s) blocked to keep trackers from loading.</p>`, blocked)
	}
	if cut {
		b.WriteString(`<p class="mr-note">This email was too long to keep in full; the end is cut off.</p>`)
	}
	b.WriteString(`</div><div class="mr-mail">` + content + "</div>\n</body></html>\n")

	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(truncate(v.subject, 60), ""))
	if name = strings.Trim(name, " ."); name == "" {
		name = "email"
	}
	return name + ".html", b.Bytes()
}

var urlRE = regexp.MustCompile(`https?://[^\s<>"]*[^\s<>".,;:!?)\]'}]`)

// linkify escapes plain text and turns its URLs into links.
func linkify(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range urlRE.FindAllStringIndex(s, -1) {
		u := html.EscapeString(s[m[0]:m[1]])
		b.WriteString(html.EscapeString(s[last:m[0]]))
		b.WriteString(`<a href="` + u + `">` + u + "</a>")
		last = m[1]
	}
	b.WriteString(html.EscapeString(s[last:]))
	return b.String()
}

// Elements removed together with their content.
var droppedBlocks = []string{"script", "iframe", "frame", "frameset", "object", "embed", "applet", "title", "template", "noscript"}

// Tags removed with their content kept: the page brings its own document
// structure, and nothing in the mail may change how it loads or animate a link.
var droppedTags = map[string]bool{
	"html": true, "head": true, "body": true, "meta": true, "link": true, "base": true,
	"animate": true, "set": true, "animatemotion": true, "animatetransform": true,
}

// urlAttrs hold a URL a browser may follow or load.
var urlAttrs = map[string]bool{
	"href": true, "src": true, "xlink:href": true, "background": true, "poster": true,
	"lowsrc": true, "dynsrc": true, "data": true, "cite": true, "longdesc": true,
}

// sanitizeHTML makes a mail's HTML safe to open from the chat. The page's CSP
// is the real barrier; this pass also removes active content and remote images
// so the file stays harmless in viewers that ignore CSP. It returns how many
// remote images it blocked.
func sanitizeHTML(s string) (string, int) {
	s = dropBlocks(s, droppedBlocks...)
	var b strings.Builder
	b.Grow(len(s))
	blocked := 0
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		s = s[i:]
		if strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?") { // comments (incl. [if mso]), doctype
			end := "-->"
			if !strings.HasPrefix(s, "<!--") {
				end = ">"
			}
			j := strings.Index(s, end)
			if j < 0 {
				break
			}
			s = s[j+len(end):]
			continue
		}
		t, rest, ok := parseTag(s)
		s = rest
		if !ok {
			b.WriteString("&lt;")
			continue
		}
		if droppedTags[t.name] || slices.Contains(droppedBlocks, t.name) {
			continue
		}
		b.WriteString(t.render(&blocked))
	}
	return b.String(), blocked
}

type tagAttr struct {
	name, val string // name lower case, val still entity-encoded
	hasVal    bool
}

type htmlTag struct {
	name    string // lower case, without the slash
	closing bool
	attrs   []tagAttr
}

// parseTag reads the tag at the start of s. Without a tag there, ok is false
// and rest is s past the '<'. An unterminated tag consumes the rest of s.
func parseTag(s string) (t htmlTag, rest string, ok bool) {
	i := 1
	if i < len(s) && s[i] == '/' {
		t.closing = true
		i++
	}
	start := i
	for i < len(s) && isNameByte(s[i]) {
		i++
	}
	if i == start || !isLetter(s[start]) {
		return t, s[1:], false
	}
	t.name = asciiLower(s[start:i])
	for {
		for i < len(s) && (isSpace(s[i]) || s[i] == '/') {
			i++
		}
		if i >= len(s) {
			return t, "", true
		}
		if s[i] == '>' {
			return t, s[i+1:], true
		}
		ns := i
		for i < len(s) && !isSpace(s[i]) && s[i] != '=' && s[i] != '>' && s[i] != '/' {
			i++
		}
		a := tagAttr{name: asciiLower(s[ns:i])}
		j := i
		for j < len(s) && isSpace(s[j]) {
			j++
		}
		if j < len(s) && s[j] == '=' {
			j++
			for j < len(s) && isSpace(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '"' || s[j] == '\'') {
				k := strings.IndexByte(s[j+1:], s[j])
				if k < 0 {
					return t, "", true
				}
				a.val, i = s[j+1:j+1+k], j+2+k
			} else {
				vs := j
				for j < len(s) && !isSpace(s[j]) && s[j] != '>' {
					j++
				}
				a.val, i = s[vs:j], j
			}
			a.hasVal = true
		}
		if a.name != "" {
			t.attrs = append(t.attrs, a)
		}
	}
}

// render writes the tag back out with only safe attributes, all re-escaped.
func (t htmlTag) render(blocked *int) string {
	var b strings.Builder
	b.WriteByte('<')
	if t.closing {
		b.WriteString("/" + t.name + ">")
		return b.String()
	}
	b.WriteString(t.name)
	for _, a := range t.attrs {
		val := html.UnescapeString(a.val)
		name := a.name
		switch {
		case strings.HasPrefix(name, "on"), name == "srcset", name == "ping", name == "action",
			name == "formaction", name == "rel" && t.name == "a":
			continue
		case name == "style":
			if low := strings.ToLower(val); strings.Contains(low, "expression") || strings.Contains(low, "javascript:") {
				continue
			}
		case urlAttrs[name]:
			u := strings.ToLower(strings.Map(func(r rune) rune {
				if r <= ' ' || r == 0x7f {
					return -1
				}
				return r
			}, val))
			remote := strings.HasPrefix(u, "http:") || strings.HasPrefix(u, "https:") || strings.HasPrefix(u, "//")
			switch {
			case strings.HasPrefix(u, "data:image/") && t.name == "img" && name == "src":
			case strings.HasPrefix(u, "javascript:"), strings.HasPrefix(u, "vbscript:"), strings.HasPrefix(u, "data:"):
				continue
			case remote && t.name == "img" && name == "src":
				*blocked++
				name = "data-blocked-src" // keeps the layout's alt text, loads nothing
			case remote && name != "href" && name != "cite":
				continue
			}
		}
		b.WriteString(" " + name)
		if a.hasVal {
			b.WriteString(`="` + html.EscapeString(val) + `"`)
		}
	}
	if t.name == "a" {
		b.WriteString(` rel="noopener noreferrer"`)
	}
	b.WriteByte('>')
	return b.String()
}

// clip cuts s to at most n UTF-16 units, preferring a word boundary and
// marking the cut with an ellipsis.
func clip(s string, n int) (string, bool) {
	if u16len(s) <= n {
		return s, false
	}
	if n < 2 {
		return "", true
	}
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xffff {
			w = 2
		}
		if units+w > n-1 { // one unit for the ellipsis
			cut := s[:i]
			if j := strings.LastIndexAny(cut, " \n"); j > len(cut)*85/100 {
				cut = cut[:j]
			}
			return strings.TrimRight(cut, " \n") + "…", true
		}
		units += w
	}
	return s, false
}

// u16len counts UTF-16 code units, the unit of Telegram's length limits.
func u16len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func byteAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func isLetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }
func isNameByte(c byte) bool {
	return isLetter(c) || '0' <= c && c <= '9' || c == '-' || c == ':' || c == '_'
}
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// ------------------------------------------------------------ telegram

// sendMessage posts an HTML-formatted message and returns its id, so the
// documents that follow can be threaded under it.
func sendMessage(text string) (int64, error) {
	payload, err := json.Marshal(map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	})
	if err != nil {
		return 0, err
	}
	res, err := textClient.Post(apiBase+"/bot"+botToken+"/sendMessage", "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	b, err := drain(res)
	if err != nil {
		return 0, err
	}
	var reply struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	json.Unmarshal(b, &reply) // a missing id only loses the threading
	return reply.Result.MessageID, nil
}

type docOpts struct {
	replyTo int64 // 0: not a reply
	caption string
	silent  bool
}

// sendDocument streams one spooled file as multipart/form-data with an exact
// Content-Length, so nothing is copied into a second buffer.
func sendDocument(name string, sp *spool, o docOpts) error {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	fields := [][2]string{{"chat_id", chatID}}
	if o.caption != "" {
		fields = append(fields, [2]string{"caption", truncate(o.caption, maxCaptionText)})
	}
	if o.silent {
		fields = append(fields, [2]string{"disable_notification", "true"})
	}
	if o.replyTo != 0 {
		fields = append(fields, [2]string{"reply_parameters",
			fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, o.replyTo)})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return err
		}
	}
	if _, err := mw.CreateFormFile("document", name); err != nil {
		return err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n" // what mw.Close() would emit after the file

	file, err := sp.reader()
	if err != nil {
		return err
	}
	body := io.MultiReader(bytes.NewReader(head.Bytes()), file, strings.NewReader(tail))

	req, err := http.NewRequest(http.MethodPost, apiBase+"/bot"+botToken+"/sendDocument", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(head.Len()) + sp.size + int64(len(tail))

	res, err := uploadClient.Do(req)
	if err != nil {
		return err
	}
	_, err = drain(res)
	return err
}

func drain(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	b = bytes.TrimSpace(b)
	if res.StatusCode != http.StatusOK {
		return b, fmt.Errorf("%s: %s", res.Status, b)
	}
	debugf("telegram %s: %s", res.Status, b)
	return b, nil
}

func debugf(format string, a ...any) {
	if debugOn {
		log.Printf("debug: "+format, a...)
	}
}

// testUpload sends one local file to the configured chat and reports the reply.
func testUpload(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		log.Fatal(err)
	}
	debugOn = true
	name := safeFilename(fi.Name(), 1)
	log.Printf("uploading %q (%d bytes) to chat %s", name, fi.Size(), chatID)
	if err := sendDocument(name, &spool{f: f, size: fi.Size()}, docOpts{}); err != nil {
		log.Fatalf("FAILED: %v", err)
	}
	log.Println("OK: Telegram accepted the upload")
}

// -------------------------------------------------------------- parsing

// decodeReader undoes the content transfer encoding, if any is left to undo.
// multipart.NextPart already decodes quoted-printable and drops that header.
func decodeReader(r io.Reader, cte string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &padReader{r: r})
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// padReader supplies the "=" padding some mailers omit, so the strict decoder
// does not fail on the last quantum. It adds nothing to properly padded input.
type padReader struct {
	r    io.Reader
	n    int // base64 characters seen so far
	pad  []byte
	done bool
}

func (p *padReader) Read(b []byte) (int, error) {
	if !p.done {
		n, err := p.r.Read(b)
		for _, c := range b[:n] {
			if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
				p.n++
			}
		}
		if err == nil || n > 0 {
			return n, err
		}
		if err != io.EOF {
			return n, err
		}
		p.done = true
		switch p.n % 4 {
		case 2:
			p.pad = []byte("==")
		case 3:
			p.pad = []byte("=")
		}
	}
	if len(p.pad) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.pad)
	p.pad = p.pad[n:]
	return n, nil
}

// readText decodes transfer encoding and charset, reading at most limit bytes.
// cut reports that the part was longer than that.
func readText(r io.Reader, cte, charset string, limit int64) (s string, cut bool) {
	r = decodeReader(r, cte)
	if cr, err := charsetReader(charset, r); err == nil {
		r = cr
	}
	b, _ := io.ReadAll(io.LimitReader(r, limit+1))
	if cut = int64(len(b)) > limit; cut {
		b = b[:limit]
	}
	s = strings.ToValidUTF8(string(b), "")
	return strings.TrimSpace(strings.ReplaceAll(s, "\r", "")), cut
}

// lineBreakTags end a line when an HTML mail is flattened to text.
var lineBreakTags = map[string]bool{
	"br": true, "p": true, "div": true, "tr": true, "li": true, "hr": true, "pre": true,
	"table": true, "ul": true, "ol": true, "blockquote": true, "section": true, "article": true,
	"header": true, "footer": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// htmlToText flattens HTML for mails that carry no text/plain part, keeping
// the line structure that block elements imply.
func htmlToText(s string) string {
	if s == "" {
		return ""
	}
	s = dropBlocks(s, "head", "script", "style", "title")
	flat := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ") // source line breaks are not text
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(flat.Replace(s))
			break
		}
		b.WriteString(flat.Replace(s[:i]))
		s = s[i:]
		if strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?") { // comments, doctype
			end := "-->"
			if !strings.HasPrefix(s, "<!--") {
				end = ">"
			}
			j := strings.Index(s, end)
			if j < 0 {
				break
			}
			s = s[j+len(end):]
			continue
		}
		t, rest, ok := parseTag(s)
		if !ok {
			b.WriteByte('<')
			s = s[1:]
			continue
		}
		switch {
		case lineBreakTags[t.name]:
			b.WriteByte('\n')
		case t.name == "td" || t.name == "th":
			b.WriteByte(' ')
		}
		s = rest
	}
	return tidyText(html.UnescapeString(b.String()), true)
}

// dropBlocks removes the named elements (lower case) together with their
// content. One that is never closed takes the rest of the document with it.
func dropBlocks(s string, names ...string) string {
	low := asciiLower(s)
	var b strings.Builder
	last := 0
	for i := 0; ; {
		j := strings.IndexByte(low[i:], '<')
		if j < 0 {
			break
		}
		i += j
		name := ""
		for _, n := range names {
			if strings.HasPrefix(low[i+1:], n) && !isNameByte(byteAt(low, i+1+len(n))) {
				name = n
				break
			}
		}
		if name == "" {
			i++
			continue
		}
		b.WriteString(s[last:i])
		end := strings.Index(low[i:], "</"+name)
		if end < 0 {
			return b.String()
		}
		gt := strings.IndexByte(low[i+end:], '>')
		if gt < 0 {
			return b.String()
		}
		i += end + gt + 1
		last = i
	}
	b.WriteString(s[last:])
	return b.String()
}

// tidyText trims lines and keeps at most one blank line in a row. collapse,
// for text that came out of HTML, also squeezes spaces and strips the
// invisible padding marketing mail uses to fill its preheader.
func tidyText(s string, collapse bool) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\u00ad', '\u200b', '\u2060', '\ufeff':
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		if collapse {
			words := strings.Fields(l) // NBSP counts as a space here
			kept := words[:0]
			for _, w := range words {
				// ZWNJ inside a word is real (Persian); at its edges it is padding
				if w = strings.Trim(w, "\u200c\u034f"); w != "" {
					kept = append(kept, w)
				}
			}
			l = strings.Join(kept, " ")
		} else {
			l = strings.TrimRight(l, " \t")
		}
		if strings.TrimSpace(l) == "" {
			if blank++; blank > 1 {
				continue
			}
			l = ""
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// mailDate formats the Date header in the relay's time zone, falling back to
// the time of receipt.
func mailDate(h string) string {
	t, err := mail.ParseDate(h)
	if err != nil {
		t = time.Now()
	}
	return t.Local().Format("2 Jan 2006, 15:04 MST")
}

func firstAddress(raw string, dec *mime.WordDecoder) string {
	if raw == "" {
		return ""
	}
	ap := mail.AddressParser{WordDecoder: dec}
	if list, err := ap.ParseList(raw); err == nil && len(list) > 0 {
		return list[0].Address
	}
	return bracketed(raw)
}

func decodeHeader(dec *mime.WordDecoder, v string) string {
	if v == "" {
		return ""
	}
	if s, err := dec.DecodeHeader(v); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(v)
}

// -------------------------------------------------------------- helpers

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func truncate(s string, maxChars int) string {
	if len(s) <= maxChars { // bytes <= chars implies runes <= maxChars
		return s
	}
	n := 0
	for i := range s {
		if n == maxChars {
			return s[:i]
		}
		n++
	}
	return s
}

// safeFilename strips path separators and control characters from a MIME filename.
func safeFilename(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if s = strings.TrimSpace(s); s == "" || s == "." || s == ".." {
		return fmt.Sprintf("attachment-%d.bin", n)
	}
	return truncate(s, 100)
}

// charsetReader supports the charsets that need no external tables.
func charsetReader(charset string, r io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "iso-8859-1", "iso8859-1", "latin1", "windows-1252", "cp1252":
		return &latin1Reader{r: r}, nil
	default: // utf-8, us-ascii and anything unknown pass through
		return r, nil
	}
}

type latin1Reader struct {
	r    io.Reader
	buf  [4]byte
	i, n int
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	var one [1]byte
	n := 0
	for n < len(p) {
		if l.i < l.n {
			p[n] = l.buf[l.i]
			l.i++
			n++
			continue
		}
		m, err := l.r.Read(one[:])
		if m > 0 {
			l.n = utf8.EncodeRune(l.buf[:], rune(one[0]))
			l.i = 0
			continue
		}
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
	}
	return n, nil
}
