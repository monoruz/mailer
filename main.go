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
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSessionBytes  = 40 << 20 // hard cap on bytes read per connection
	maxCmdLine       = 1024
	maxTextCapture   = 16 << 10 // bytes of text/plain kept while streaming
	maxHTMLCapture   = 64 << 10 // only read when there is no text/plain part
	maxBodyChars     = 1000
	maxTelegramText  = 3800
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

	body := p.text
	if body == "" {
		body = htmlToText(p.htmlText)
	}
	if body == "" {
		body = "(empty)"
	}

	var b strings.Builder
	b.WriteString("📧 <b>New Email Received</b>\n\n<b>From:</b> ")
	b.WriteString(escape(orDefault(decodeHeader(dec, msg.Header.Get("From")), "Unknown")))
	b.WriteString("\n<b>To:</b> ")
	b.WriteString(escape(orDefault(decodeHeader(dec, rawTo), "Unknown")))
	b.WriteString("\n<b>Subject:</b> ")
	b.WriteString(escape(orDefault(decodeHeader(dec, msg.Header.Get("Subject")), "(No subject)")))
	b.WriteString("\n\n")
	b.WriteString(escape(p.attachmentLine()))
	b.WriteString("\n\n<b>Body</b>\n<pre>")
	b.WriteString(escape(body))
	b.WriteString("</pre>")

	sendMessage(truncate(b.String(), maxTelegramText))

	sent := 0
	for _, a := range p.atts {
		if a.sp == nil || a.oversize {
			continue
		}
		if err := sendDocument(a); err != nil {
			log.Printf("sendDocument %s (%d bytes): %v", a.name, a.sp.size, err)
			continue
		}
		sent++
	}
	log.Printf("mail to=%s attachments=%d forwarded=%d", toAddr, p.count, sent)
	return nil
}

func (p *parsed) attachmentLine() string {
	if p.count == 0 {
		return "📎 None"
	}
	line := fmt.Sprintf("📎 %d attachment(s)", p.count)
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
		line += ": " + strings.Join(names, ", ")
	}
	return line
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
		p.text = truncate(readText(body, h.Get("Content-Transfer-Encoding"), params["charset"], maxTextCapture), maxBodyChars)
	case mediatype == "text/html" && p.htmlText == "":
		p.htmlText = readText(body, h.Get("Content-Transfer-Encoding"), params["charset"], maxHTMLCapture)
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

// ------------------------------------------------------------ telegram

func sendMessage(text string) {
	payload, err := json.Marshal(struct {
		ChatID                string `json:"chat_id"`
		Text                  string `json:"text"`
		ParseMode             string `json:"parse_mode"`
		DisableWebPagePreview bool   `json:"disable_web_page_preview"`
	}{chatID, text, "HTML", true})
	if err != nil {
		log.Println("telegram:", err)
		return
	}
	res, err := textClient.Post(apiBase+"/bot"+botToken+"/sendMessage", "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Println("telegram:", err)
		return
	}
	drain(res)
}

// sendDocument streams one spooled attachment as multipart/form-data with an
// exact Content-Length, so nothing is copied into a second buffer.
func sendDocument(a attachment) error {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("chat_id", chatID); err != nil {
		return err
	}
	if _, err := mw.CreateFormFile("document", a.name); err != nil {
		return err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n" // what mw.Close() would emit after the file

	file, err := a.sp.reader()
	if err != nil {
		return err
	}
	body := io.MultiReader(bytes.NewReader(head.Bytes()), file, strings.NewReader(tail))

	req, err := http.NewRequest(http.MethodPost, apiBase+"/bot"+botToken+"/sendDocument", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(head.Len()) + a.sp.size + int64(len(tail))

	res, err := uploadClient.Do(req)
	if err != nil {
		return err
	}
	return drain(res)
}

func drain(res *http.Response) error {
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", res.Status, bytes.TrimSpace(b))
	}
	debugf("telegram %s: %s", res.Status, bytes.TrimSpace(b))
	return nil
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
	if err := sendDocument(attachment{name: name, sp: &spool{f: f, size: fi.Size()}}); err != nil {
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
func readText(r io.Reader, cte, charset string, limit int64) string {
	r = decodeReader(r, cte)
	if cr, err := charsetReader(charset, r); err == nil {
		r = cr
	}
	b, _ := io.ReadAll(io.LimitReader(r, limit))
	s := strings.ToValidUTF8(string(b), "")
	return strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
}

// htmlToText is a crude fallback for mails that carry no text/plain part.
func htmlToText(s string) string {
	if s == "" {
		return ""
	}
	s = dropBlocks(s, "<script", "</script>")
	s = dropBlocks(s, "<style", "</style>")
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
			b.WriteByte(' ')
		case depth == 0:
			b.WriteRune(r)
		}
	}
	out := html.UnescapeString(b.String())
	return truncate(strings.TrimSpace(strings.Join(strings.Fields(out), " ")), maxBodyChars)
}

func dropBlocks(s, open, close string) string {
	for {
		i := strings.Index(strings.ToLower(s), open)
		if i < 0 {
			return s
		}
		j := strings.Index(strings.ToLower(s[i:]), close)
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len(close):]
	}
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
