package main

// Web inbox and settings, server-rendered with html/template and driven by
// htmx, which is embedded in the binary. There is no login: the UI binds to
// localhost by default and is meant to sit behind Cloudflare Zero Trust (or
// another authenticating proxy).
//
// Every state-changing request must carry the HX-Request header, which a
// cross-site form cannot set, and must not be a cross-site fetch. That is the
// CSRF defence, since the proxy's session cookie rides along on any request.
// /mcp is the exception: it has its own checks (see mcp.go).

import (
	"embed"
	"errors"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/htmx.min.js web/app.js web/*.html
var webFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"id":    idHex,
	"esc":   url.PathEscape,
	"human": humanSize,
	"ago":   ago,
	"join":  strings.Join,
	"left": func(t time.Time) string {
		d := time.Until(t.Add(mailTTL))
		if d < time.Hour {
			return "<1h"
		}
		if d < 48*time.Hour {
			return strconv.Itoa(int(d.Hours())) + "h"
		}
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	},
}).ParseFS(webFS, "web/*.html"))

func serveWeb(addr string) {
	mux := http.NewServeMux()
	for _, name := range []string{"htmx.min.js", "app.js"} {
		b, _ := webFS.ReadFile("web/" + name)
		mux.HandleFunc("GET /static/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(b)
		})
	}

	mux.HandleFunc("GET /{$}", inboxPage)
	mux.HandleFunc("GET /mail/{id}", mailPage)
	mux.HandleFunc("GET /mail/{id}/html", mailHTML)
	mux.HandleFunc("GET /mail/{id}/att/{n}", mailAttachment)
	mux.HandleFunc("POST /mail/{id}/reply", mailReply)
	mux.HandleFunc("POST /mail/{id}/forward", mailForward)
	mux.HandleFunc("DELETE /mail/{id}", mailDelete)

	mux.HandleFunc("GET /compose", composePage)
	mux.HandleFunc("POST /compose", composeSend)
	mux.HandleFunc("GET /sent", sentPage)
	mux.HandleFunc("GET /sent/{id}", sentMailPage)
	mux.HandleFunc("GET /sent/{id}/att/{n}", sentAttachment)
	mux.HandleFunc("DELETE /sent/{id}", sentDelete)

	mux.HandleFunc("POST /mcp", mcpHandler)
	mux.HandleFunc("/mcp", mcpOther)

	mux.HandleFunc("GET /blocked", rulesPage(bBlocked))
	mux.HandleFunc("POST /blocked", ruleCreate(bBlocked))
	mux.HandleFunc("DELETE /blocked/{key}", ruleDelete(bBlocked))
	mux.HandleFunc("GET /domains", rulesPage(bDomains))
	mux.HandleFunc("POST /domains", ruleCreate(bDomains))
	mux.HandleFunc("DELETE /domains/{key}", ruleDelete(bDomains))

	mux.HandleFunc("GET /dests", destsPage)
	mux.HandleFunc("POST /dests", destSave)
	mux.HandleFunc("GET /dests/{id}/edit", destEdit)
	mux.HandleFunc("GET /dests/{id}", destRow)
	mux.HandleFunc("PUT /dests/{id}", destSave)
	mux.HandleFunc("POST /dests/{id}/toggle", destToggle)
	mux.HandleFunc("POST /dests/{id}/test", destTest)
	mux.HandleFunc("DELETE /dests/{id}", destDelete)

	srv := &http.Server{
		Addr:              addr,
		Handler:           guard(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // compose uploads attachments
		WriteTimeout:      2 * time.Minute, // a reply waits for the remote MX
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	log.Printf("web UI listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

// guard sets the security headers and enforces the CSRF rule above.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if !strings.HasPrefix(r.URL.Path, "/mail/") || !strings.HasSuffix(r.URL.Path, "/html") {
			h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-src 'self'; form-action 'self'; base-uri 'none'")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			limit := int64(256 << 10)
			if r.URL.Path == "/compose" || r.URL.Path == "/mcp" {
				limit = uploadLimit()
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			if r.URL.Path != "/mcp" && (r.Header.Get("HX-Request") != "true" || r.Header.Get("Sec-Fetch-Site") == "cross-site") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("web: template %s: %v", name, err)
	}
}

// page is the layout's data; Data.Body names the content template, since
// html/template cannot call a template by a computed name.
type page struct {
	Title  string
	Nav    string
	Unread int
	Data   any
}

func renderPage(w http.ResponseWriter, nav, title, body string, data any) {
	render(w, "layout", page{Title: title, Nav: nav, Unread: db.unreadCount(), Data: struct {
		Body string
		D    any
	}{body, data}})
}

// flash is a one-line result shown in place of a form or button.
func flash(w http.ResponseWriter, ok bool, msg string) {
	render(w, "flash", struct {
		OK  bool
		Msg string
	}{ok, msg})
}

func notFound(w http.ResponseWriter) { http.Error(w, "not found", http.StatusNotFound) }

// uploadLimit bounds a compose or MCP request: the stored attachment cap, as
// base64 for MCP, plus room for the text.
func uploadLimit() int64 { return storeMaxBytes*4/3 + 2<<20 }

// ---------------------------------------------------------------- inbox

func inboxPage(w http.ResponseWriter, r *http.Request) {
	before, _ := parseID(r.URL.Query().Get("before"))
	q := r.URL.Query().Get("q")
	rows, more, err := db.listMails(before, q, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := struct {
		Rows  []Mail
		More  bool
		Next  uint64
		Q     string
		Paged bool
	}{Rows: rows, More: more, Q: q, Paged: before != 0}
	if more && len(rows) > 0 {
		data.Next = rows[len(rows)-1].ID
	}
	// The list polls itself; serve only the rows then.
	if r.Header.Get("HX-Target") == "mail-rows" {
		render(w, "mail-rows", data)
		return
	}
	renderPage(w, "inbox", "Inbox", "inbox", data)
}

func loadMail(w http.ResponseWriter, r *http.Request) (*Mail, bool) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		notFound(w)
		return nil, false
	}
	m, err := db.getMail(id)
	if err != nil {
		notFound(w)
		return nil, false
	}
	return m, true
}

func mailPage(w http.ResponseWriter, r *http.Request) {
	m, ok := loadMail(w, r)
	if !ok {
		return
	}
	b, err := db.getBody(m.ID)
	if err != nil {
		notFound(w)
		return
	}
	if !m.Read {
		db.updateMail(m.ID, func(x *Mail) { x.Read = true })
	}
	dests := []Dest{}
	if list, err := db.listDests(); err == nil {
		for _, d := range list {
			if d.Kind == kindEmail {
				dests = append(dests, d)
			}
		}
	}
	renderPage(w, "inbox", m.Subject, "mail", struct {
		M         *Mail
		Text      string
		HasHTML   bool
		Cut       bool
		ReplyTo   string
		ReplySubj string
		Forwards  []Dest
	}{
		M:         m,
		Text:      plainBody(b),
		HasHTML:   b.HTML != "",
		Cut:       b.TextCut || b.HTMLCut,
		ReplyTo:   replyAddress(m),
		ReplySubj: prefixed("Re:", m.Subject),
		Forwards:  dests,
	})
}

// mailHTML serves the sanitized original HTML for a sandboxed iframe. The
// CSP blocks scripts and every remote load, so trackers stay silent.
func mailHTML(w http.ResponseWriter, r *http.Request) {
	m, ok := loadMail(w, r)
	if !ok {
		return
	}
	b, err := db.getBody(m.ID)
	if err != nil || b.HTML == "" {
		notFound(w)
		return
	}
	clean, blocked := sanitizeHTML(b.HTML)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'; sandbox allow-popups allow-popups-to-escape-sandbox")
	var sb strings.Builder
	sb.WriteString(`<!doctype html><meta charset="utf-8"><base target="_blank"><style>body{margin:0;padding:12px;font:15px/1.5 sans-serif;background:#fff;color:#111}</style>`)
	if blocked > 0 {
		sb.WriteString(`<p style="font:12px sans-serif;color:#888;margin:0 0 8px">` + strconv.Itoa(blocked) + ` remote image(s) blocked.</p>`)
	}
	sb.WriteString(clean)
	w.Write([]byte(sb.String()))
}

func mailAttachment(w http.ResponseWriter, r *http.Request) {
	m, ok := loadMail(w, r)
	if !ok {
		return
	}
	serveAttachment(w, r, m)
}

func serveAttachment(w http.ResponseWriter, r *http.Request, m *Mail) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= len(m.Files) || !m.Files[n].Stored {
		notFound(w)
		return
	}
	// Always a download, never rendered inline: an HTML or SVG attachment
	// must not run with the UI's origin.
	err = db.writeAttachment(m.ID, n, func(size int) io.Writer {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": m.Files[n].Name}))
		w.Header().Set("Content-Length", strconv.Itoa(size))
		return w
	})
	if errors.Is(err, errNotFound) {
		notFound(w)
	}
}

func mailReply(w http.ResponseWriter, r *http.Request) {
	m, ok := loadMail(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(r.FormValue("to")) == "" {
		flash(w, false, "To: not a valid email address")
		return
	}
	s, err := sendReply(m, r.FormValue("to"), r.FormValue("subject"), r.FormValue("body"))
	if err != nil {
		flash(w, false, userMessage(err))
		return
	}
	flash(w, true, "Reply sent to "+s.ToAddr+".")
}

func mailForward(w http.ResponseWriter, r *http.Request) {
	m, ok := loadMail(w, r)
	if !ok {
		return
	}
	s, err := queueForward(m, r.FormValue("to"))
	if err != nil {
		flash(w, false, userMessage(err))
		return
	}
	flash(w, true, "Forward to "+s.ToAddr+" queued; the result shows in this mail's log and in Sent.")
}

func mailDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	if err := db.deleteMail(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("back") == "1" {
		w.Header().Set("HX-Location", "/")
	}
	w.WriteHeader(http.StatusOK) // empty body: htmx removes the row
}

// -------------------------------------------------------------- compose

func composePage(w http.ResponseWriter, r *http.Request) {
	from := defaultFrom()
	local, dom, _ := strings.Cut(from, "@")
	renderPage(w, "compose", "Compose", "compose", struct {
		Local, Domain string
		Domains       []string
		To, Subject   string
		Limit         string
		Max           int
	}{local, dom, fromDomains(), r.URL.Query().Get("to"), r.URL.Query().Get("subject"),
		humanSize(storeMaxBytes), maxComposeFiles})
}

func composeSend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		flash(w, false, "Upload too large: attachments may total "+humanSize(storeMaxBytes)+".")
		return
	}
	defer r.MultipartForm.RemoveAll()
	var files []upload
	for _, fh := range r.MultipartForm.File["files"] {
		if fh.Size == 0 && fh.Filename == "" {
			continue // the empty input of a form sent without files
		}
		f, err := fh.Open()
		if err != nil {
			flash(w, false, "Could not read "+fh.Filename+".")
			return
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			flash(w, false, "Could not read "+fh.Filename+".")
			return
		}
		files = append(files, upload{Name: fh.Filename, Type: fh.Header.Get("Content-Type"), Data: data})
	}
	from := strings.TrimSpace(r.FormValue("local"))
	if from != "" {
		from += "@" + r.FormValue("domain")
	}
	m, err := sendNew(from, r.FormValue("to"), r.FormValue("subject"), r.FormValue("body"), files)
	if err != nil {
		flash(w, false, userMessage(err))
		return
	}
	w.Header().Set("HX-Location", "/sent/"+idHex(m.ID))
	flash(w, true, "Sent.")
}

// ----------------------------------------------------------------- sent

func sentPage(w http.ResponseWriter, r *http.Request) {
	before, _ := parseID(r.URL.Query().Get("before"))
	q := r.URL.Query().Get("q")
	rows, more, err := db.listRows(bSent, before, q, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := struct {
		Rows []Mail
		More bool
		Next uint64
		Q    string
	}{Rows: rows, More: more, Q: q}
	if more && len(rows) > 0 {
		data.Next = rows[len(rows)-1].ID
	}
	renderPage(w, "sent", "Sent", "sent", data)
}

func loadSent(w http.ResponseWriter, r *http.Request) (*Mail, bool) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		notFound(w)
		return nil, false
	}
	m, err := db.getRow(bSent, id)
	if err != nil {
		notFound(w)
		return nil, false
	}
	return m, true
}

func sentMailPage(w http.ResponseWriter, r *http.Request) {
	m, ok := loadSent(w, r)
	if !ok {
		return
	}
	b, err := db.getBody(m.ID)
	if err != nil {
		notFound(w)
		return
	}
	orig := false
	if m.Orig != 0 {
		_, err := db.getMail(m.Orig)
		orig = err == nil
	}
	renderPage(w, "sent", orDefault(m.Subject, "(no subject)"), "sent-mail", struct {
		M       *Mail
		Text    string
		HasOrig bool
	}{m, b.Text, orig})
}

func sentAttachment(w http.ResponseWriter, r *http.Request) {
	m, ok := loadSent(w, r)
	if !ok {
		return
	}
	serveAttachment(w, r, m)
}

func sentDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	if err := db.deleteRow(bSent, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("back") == "1" {
		w.Header().Set("HX-Location", "/sent")
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------- rules

type rulesView struct {
	Kind, Path, Title, Hint, Placeholder string
	Rules                                []Rule
}

func rulesMeta(bucket []byte) rulesView {
	if string(bucket) == string(bBlocked) {
		return rulesView{Kind: "blocked", Path: "/blocked", Title: "Blocked senders",
			Hint:        "Mail from a matching sender is dropped and never stored. Use an address, a domain (includes subdomains), or a glob like *@spam.io.",
			Placeholder: "*@spam.io"}
	}
	return rulesView{Kind: "domains", Path: "/domains", Title: "Allowed domains",
		Hint:        "Mail is accepted only when its recipient is at one of these domains. With none, every mail is dropped.",
		Placeholder: "example.com"}
}

func rulesPage(bucket []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := rulesMeta(bucket)
		v.Rules, _ = db.listRules(bucket)
		renderPage(w, v.Kind, v.Title, "rules", v)
	}
}

func ruleCreate(bucket []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := rulesMeta(bucket)
		var key string
		var err error
		if v.Kind == "blocked" {
			key, err = normPattern(r.FormValue("key"))
		} else {
			key, err = normDomain(r.FormValue("key"))
		}
		if err != nil {
			w.Header().Set("HX-Retarget", "#form-msg")
			w.Header().Set("HX-Reswap", "innerHTML")
			flash(w, false, err.Error())
			return
		}
		rule := Rule{Key: key, Note: truncate(strings.TrimSpace(r.FormValue("note")), 200)}
		if err := db.putRule(bucket, rule); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		v.Rules, _ = db.listRules(bucket)
		render(w, "rule-rows", v)
	}
}

func ruleDelete(bucket []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := db.deleteRule(bucket, r.PathValue("key")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// --------------------------------------------------------- destinations

func destsPage(w http.ResponseWriter, r *http.Request) {
	list, _ := db.listDests()
	renderPage(w, "dests", "Destinations", "dests", list)
}

func loadDest(w http.ResponseWriter, r *http.Request) (*Dest, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return nil, false
	}
	d, err := db.getDest(id)
	if err != nil {
		notFound(w)
		return nil, false
	}
	return d, true
}

func destRow(w http.ResponseWriter, r *http.Request) {
	if d, ok := loadDest(w, r); ok {
		render(w, "dest-row", d)
	}
}

func destEdit(w http.ResponseWriter, r *http.Request) {
	if d, ok := loadDest(w, r); ok {
		render(w, "dest-edit", d)
	}
}

// destSave creates (POST /dests) or updates (PUT /dests/{id}) a destination.
func destSave(w http.ResponseWriter, r *http.Request) {
	d := &Dest{Enabled: true}
	if r.Method == http.MethodPut {
		old, ok := loadDest(w, r)
		if !ok {
			return
		}
		d = old
	}
	kind := r.FormValue("kind")
	target, err := normDest(kind, r.FormValue("target"))
	if err == nil {
		d.Senders, err = splitPatterns(r.FormValue("senders"))
	}
	if err != nil {
		slot := "#form-msg"
		if d.ID != 0 {
			slot += "-" + strconv.FormatUint(d.ID, 10)
		}
		w.Header().Set("HX-Retarget", slot)
		w.Header().Set("HX-Reswap", "innerHTML")
		flash(w, false, err.Error())
		return
	}
	d.Kind, d.Target = kind, target
	d.Name = truncate(strings.TrimSpace(r.FormValue("name")), 80)
	if err := db.putDest(d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodPut {
		render(w, "dest-row", d)
		return
	}
	list, _ := db.listDests()
	render(w, "dest-rows", list)
}

func destToggle(w http.ResponseWriter, r *http.Request) {
	d, ok := loadDest(w, r)
	if !ok {
		return
	}
	d.Enabled = !d.Enabled
	if err := db.putDest(d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "dest-row", d)
}

func destDelete(w http.ResponseWriter, r *http.Request) {
	d, ok := loadDest(w, r)
	if !ok {
		return
	}
	if err := db.deleteDest(d.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func destTest(w http.ResponseWriter, r *http.Request) {
	d, ok := loadDest(w, r)
	if !ok {
		return
	}
	var err error
	switch d.Kind {
	case kindTelegram:
		_, err = sendMessage(d.Target, "✅ Test message from mailer ("+escape(hostname)+")")
	case kindEmail:
		from := forwardFromAddr
		if from == "" {
			for dom := range rules.Load().domains {
				from = "mailer@" + dom
				break
			}
		}
		if from == "" {
			err = errors.New("add an allowed domain or set FORWARD_FROM first")
			break
		}
		err = deliver(&outMsg{From: from, To: d.Target, Subject: "Test from mailer",
			Text: "This is a test message from the mailer on " + hostname + "."})
	}
	if err != nil {
		flash(w, false, "Test failed: "+err.Error())
		return
	}
	flash(w, true, "Test sent.")
}

// -------------------------------------------------------------- helpers

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d < 7*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2")
}
