package main

// MCP (Model Context Protocol) server at /mcp, over the Streamable HTTP
// transport: each JSON-RPC request is a POST answered with one JSON response.
// It is stateless, so no session id is issued and there is no SSE stream.
//
// /mcp has no authentication of its own. Put it behind Cloudflare Access with
// a Service Auth policy (the client then sends CF-Access-Client-Id and
// CF-Access-Client-Secret). A Bypass policy leaves it open to anyone, who can
// then read the inbox and send mail from every allowed domain.
//
// Browsers are kept out: the body must be application/json, which a
// cross-site page cannot send without a CORS preflight that is never
// answered, and a request with a foreign Origin is refused (DNS rebinding).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const mcpMaxAttachment = 5 << 20 // get_attachment returns at most this many bytes

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpOther answers GET (no server-initiated stream) and DELETE (no sessions).
func mcpOther(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func mcpHandler(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{-32600, "request too large"}})
			return
		}
		writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	// A notification or a response from the client: nothing to answer.
	if len(req.ID) == 0 || req.Method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	res, rerr := mcpDispatch(&req)
	writeRPC(w, rpcResponse{ID: req.ID, Result: res, Error: rerr})
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// sameOrigin allows a missing Origin (every non-browser client) or one whose
// host is the host the request was made to.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func mcpDispatch(req *rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		ver := mcpVersions[0]
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				ver = v
			}
		}
		return map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "mailer", "title": "Mailer inbox", "version": "1.0.0"},
			"instructions": "A mail inbox for the domains " + strings.Join(fromDomains(), ", ") +
				". Mail ids are 16 hex characters. Received mail lives in the inbox, sent mail in sent; " +
				"both expire after " + mailTTL.String() + ". Sending goes straight to the recipient's mail server " +
				"and cannot be undone, so confirm recipients and content with the user before send_mail, reply_mail or forward_mail.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]map[string]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			list = append(list, t.def())
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		for _, t := range mcpTools {
			if t.Name == p.Name {
				return callTool(t, p.Arguments), nil
			}
		}
		return nil, &rpcError{-32602, "unknown tool: " + p.Name}
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

// ---------------------------------------------------------------- tools

type mcpTool struct {
	Name, Title, Desc string
	Props             map[string]any
	Required          []string
	ReadOnly, Destroy bool
	Run               func(args json.RawMessage) (*toolResult, error)
}

type toolResult struct {
	Content    []map[string]any `json:"content"`
	Structured any              `json:"structuredContent,omitempty"`
	IsError    bool             `json:"isError,omitempty"`
}

func (t *mcpTool) def() map[string]any {
	schema := map[string]any{"type": "object", "properties": t.Props, "additionalProperties": false}
	if len(t.Required) > 0 {
		schema["required"] = t.Required
	}
	desc := t.Desc
	if t.Name == "send_mail" {
		if doms := fromDomains(); len(doms) > 0 {
			desc += " Allowed sender domains: " + strings.Join(doms, ", ") + "; default sender " + defaultFrom() + "."
		}
	}
	return map[string]any{
		"name": t.Name, "title": t.Title, "description": desc, "inputSchema": schema,
		"annotations": map[string]any{
			"title": t.Title, "readOnlyHint": t.ReadOnly, "destructiveHint": t.Destroy,
			"idempotentHint": t.ReadOnly, "openWorldHint": !t.ReadOnly && !t.Destroy,
		},
	}
}

func callTool(t *mcpTool, args json.RawMessage) *toolResult {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	res, err := t.Run(args)
	if err != nil {
		var ue userErr
		msg := userMessage(err)
		if !errors.As(err, &ue) && !errors.Is(err, errNotFound) {
			log.Printf("mcp: %s: %v", t.Name, err)
		}
		return &toolResult{Content: []map[string]any{{"type": "text", "text": msg}}, IsError: true}
	}
	return res
}

// jsonResult returns v both as structured content and as its JSON text, for
// clients that read only the text.
func jsonResult(v any) (*toolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &toolResult{Content: []map[string]any{{"type": "text", "text": string(b)}}, Structured: v}, nil
}

func decodeArgs(args json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badInput("invalid arguments: %v", err)
	}
	return nil
}

func argID(s string) (uint64, error) {
	id, ok := parseID(strings.TrimSpace(s))
	if !ok {
		return 0, badInput("id must be 16 hex characters, as list_mails returns it")
	}
	return id, nil
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var idProp = str("Mail id (16 hex characters) from list_mails.")

// ------------------------------------------------------------ mail views

type mailSummary struct {
	ID          string `json:"id"`
	Folder      string `json:"folder"`
	From        string `json:"from"`
	To          string `json:"to"`
	Subject     string `json:"subject"`
	At          string `json:"at"`
	Read        *bool  `json:"read,omitempty"`
	Replied     bool   `json:"replied,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Attachments int    `json:"attachments,omitempty"`
}

func folderOf(bucket []byte) string {
	if string(bucket) == string(bSent) {
		return "sent"
	}
	return "inbox"
}

func summarize(m *Mail, bucket []byte) mailSummary {
	s := mailSummary{ID: idHex(m.ID), Folder: folderOf(bucket), From: m.From, To: m.To, Subject: m.Subject,
		At: m.At.Format("2006-01-02T15:04:05Z07:00"), Replied: m.Replied, Kind: m.Kind, Attachments: len(m.Files)}
	if folderOf(bucket) == "inbox" {
		read := m.Read
		s.Read = &read
	}
	return s
}

type attachmentInfo struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Type      string `json:"type,omitempty"`
	Available bool   `json:"available"`
}

// ------------------------------------------------------------ the tools

var mcpTools = []*mcpTool{
	{
		Name: "list_mails", Title: "List mail", ReadOnly: true,
		Desc: "List received (inbox) or sent mail, newest first, optionally filtered by text in sender, recipient or subject. Page with before = the last id of the previous page.",
		Props: map[string]any{
			"folder": map[string]any{"type": "string", "enum": []string{"inbox", "sent"}, "description": "Default inbox."},
			"query":  str("Case-insensitive text to find in sender, recipient or subject."),
			"before": str("Return mail older than this id, for the next page."),
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Default 20."},
		},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct {
				Folder, Query, Before string
				Limit                 int
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			bucket := bMeta
			switch a.Folder {
			case "", "inbox":
			case "sent":
				bucket = bSent
			default:
				return nil, badInput("folder must be inbox or sent")
			}
			var before uint64
			if a.Before != "" {
				var err error
				if before, err = argID(a.Before); err != nil {
					return nil, err
				}
			}
			if a.Limit <= 0 {
				a.Limit = 20
			}
			rows, more, err := db.listRows(bucket, before, a.Query, min(a.Limit, 100))
			if err != nil {
				return nil, err
			}
			out := struct {
				Mails []mailSummary `json:"mails"`
				More  bool          `json:"more"`
				Next  string        `json:"next_before,omitempty"`
			}{Mails: []mailSummary{}, More: more}
			for i := range rows {
				out.Mails = append(out.Mails, summarize(&rows[i], bucket))
			}
			if more && len(rows) > 0 {
				out.Next = idHex(rows[len(rows)-1].ID)
			}
			return jsonResult(out)
		},
	},
	{
		Name: "get_mail", Title: "Read mail", ReadOnly: true,
		Desc:     "Read one received or sent mail: headers, plain-text body (HTML is flattened to text), attachments and delivery log. It does not mark the mail read; use mark_mail for that.",
		Props:    map[string]any{"id": idProp},
		Required: []string{"id"},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct{ ID string }
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			id, err := argID(a.ID)
			if err != nil {
				return nil, err
			}
			m, bucket, err := db.findMail(id)
			if err != nil {
				return nil, err
			}
			b, err := db.getBody(id)
			if err != nil {
				return nil, errNotFound
			}
			out := struct {
				mailSummary
				Cc        string           `json:"cc,omitempty"`
				ReplyTo   string           `json:"reply_to,omitempty"`
				Date      string           `json:"date"`
				Body      string           `json:"body"`
				Truncated bool             `json:"truncated,omitempty"`
				HasHTML   bool             `json:"has_html,omitempty"`
				Files     []attachmentInfo `json:"attachments"`
				Log       []string         `json:"log,omitempty"`
				Original  string           `json:"original_id,omitempty"`
			}{mailSummary: summarize(m, bucket), Cc: m.Cc, ReplyTo: m.ReplyTo, Date: m.Date,
				Body: plainBody(b), Truncated: b.TextCut || b.HTMLCut, HasHTML: b.HTML != "",
				Files: []attachmentInfo{}, Log: m.Log}
			if m.Orig != 0 {
				out.Original = idHex(m.Orig)
			}
			for i, f := range m.Files {
				out.Files = append(out.Files, attachmentInfo{i, f.Name, f.Size, f.Type, f.Stored})
			}
			return jsonResult(out)
		},
	},
	{
		Name: "get_attachment", Title: "Read attachment", ReadOnly: true,
		Desc: "Return one stored attachment of a mail: text inline, images as image content, anything else as a base64 blob. At most 5 MB.",
		Props: map[string]any{
			"id":    idProp,
			"index": map[string]any{"type": "integer", "minimum": 0, "description": "Attachment index from get_mail."},
		},
		Required: []string{"id", "index"},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct {
				ID    string
				Index int
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			id, err := argID(a.ID)
			if err != nil {
				return nil, err
			}
			m, _, err := db.findMail(id)
			if err != nil {
				return nil, err
			}
			if a.Index < 0 || a.Index >= len(m.Files) {
				return nil, badInput("no attachment %d; the mail has %d", a.Index, len(m.Files))
			}
			f := m.Files[a.Index]
			if !f.Stored {
				return nil, badInput("%s was not kept (too large, or it belongs to the original mail)", f.Name)
			}
			if f.Size > mcpMaxAttachment {
				return nil, badInput("%s is %s; the limit here is %s", f.Name, humanSize(f.Size), humanSize(mcpMaxAttachment))
			}
			data, err := db.getAttachment(id, a.Index)
			if err != nil {
				return nil, errNotFound
			}
			ctype := orDefault(f.Type, "application/octet-stream")
			base, _, _ := mime.ParseMediaType(ctype)
			label := map[string]any{"type": "text", "text": f.Name + " (" + ctype + ", " + humanSize(f.Size) + ")"}
			var part map[string]any
			switch {
			case isTextType(base) && utf8.Valid(data):
				part = map[string]any{"type": "text", "text": string(data)}
			case strings.HasPrefix(base, "image/"):
				part = map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": base}
			default:
				part = map[string]any{"type": "resource", "resource": map[string]any{
					"uri":      "mailer://mail/" + idHex(id) + "/attachment/" + strconv.Itoa(a.Index),
					"mimeType": base,
					"blob":     base64.StdEncoding.EncodeToString(data),
				}}
			}
			return &toolResult{Content: []map[string]any{label, part}}, nil
		},
	},
	{
		Name: "send_mail", Title: "Send new mail",
		Desc: "Compose and send a new plain-text mail to one recipient, with optional attachments. It is delivered at once and kept in sent.",
		Props: map[string]any{
			"from":    str("Sender address at an allowed domain. Default: the configured sender."),
			"to":      str("Recipient address."),
			"subject": str("Subject line."),
			"body":    str("Plain-text body."),
			"attachments": map[string]any{"type": "array", "maxItems": maxComposeFiles, "items": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"name", "data_base64"},
				"properties": map[string]any{
					"name":         str("File name."),
					"content_type": str("MIME type, e.g. application/pdf."),
					"data_base64":  str("File content, base64."),
				},
			}},
		},
		Required: []string{"to", "subject", "body"},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct {
				From, To, Subject, Body string
				Attachments             []struct {
					Name        string `json:"name"`
					ContentType string `json:"content_type"`
					Data        string `json:"data_base64"`
				}
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			var files []upload
			for _, at := range a.Attachments {
				data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(at.Data))
				if err != nil {
					return nil, badInput("%s: data_base64 is not valid base64", at.Name)
				}
				files = append(files, upload{Name: at.Name, Type: at.ContentType, Data: data})
			}
			m, err := sendNew(a.From, a.To, a.Subject, a.Body, files)
			if err != nil {
				return nil, err
			}
			return jsonResult(map[string]any{"sent": true, "id": idHex(m.ID), "from": m.FromAddr, "to": m.ToAddr})
		},
	},
	{
		Name: "reply_mail", Title: "Reply to mail",
		Desc: "Reply to a received mail. The original is quoted under the text and the reply is threaded. It is sent from the address the mail was delivered to, at once, and kept in sent.",
		Props: map[string]any{
			"id":      idProp,
			"body":    str("Plain-text reply."),
			"to":      str("Recipient. Default: the mail's Reply-To, else its sender."),
			"subject": str("Default: Re: plus the original subject."),
		},
		Required: []string{"id", "body"},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct{ ID, Body, To, Subject string }
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			id, err := argID(a.ID)
			if err != nil {
				return nil, err
			}
			m, err := db.getMail(id)
			if err != nil {
				return nil, badInput("no received mail %s (only inbox mail can be replied to)", a.ID)
			}
			s, err := sendReply(m, a.To, a.Subject, a.Body)
			if err != nil {
				return nil, err
			}
			return jsonResult(map[string]any{"sent": true, "id": idHex(s.ID), "to": s.ToAddr, "subject": s.Subject})
		},
	},
	{
		Name: "forward_mail", Title: "Forward mail",
		Desc: "Forward a received mail, with its stored attachments, to an address. Delivery runs in the background with retries; get_mail on the returned sent id shows the outcome in its log.",
		Props: map[string]any{
			"id": idProp,
			"to": str("Recipient address."),
		},
		Required: []string{"id", "to"},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct{ ID, To string }
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			id, err := argID(a.ID)
			if err != nil {
				return nil, err
			}
			m, err := db.getMail(id)
			if err != nil {
				return nil, badInput("no received mail %s (only inbox mail can be forwarded)", a.ID)
			}
			s, err := queueForward(m, a.To)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"queued": true, "to": s.ToAddr}
			if s.ID != 0 {
				out["id"] = idHex(s.ID)
			}
			return jsonResult(out)
		},
	},
	{
		Name: "delete_mail", Title: "Delete mail", Destroy: true,
		Desc:     "Delete a received or sent mail with its attachments. This cannot be undone.",
		Props:    map[string]any{"id": idProp},
		Required: []string{"id"},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct{ ID string }
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			id, err := argID(a.ID)
			if err != nil {
				return nil, err
			}
			_, bucket, err := db.findMail(id)
			if err != nil {
				return nil, err
			}
			if err := db.deleteRow(bucket, id); err != nil {
				return nil, err
			}
			return jsonResult(map[string]any{"deleted": true, "id": idHex(id), "folder": folderOf(bucket)})
		},
	},
	{
		Name: "mark_mail", Title: "Mark read or unread",
		Desc: "Mark a received mail as read or unread.",
		Props: map[string]any{
			"id":   idProp,
			"read": map[string]any{"type": "boolean", "description": "true: read, false: unread."},
		},
		Required: []string{"id", "read"},
		Run: func(args json.RawMessage) (*toolResult, error) {
			var a struct {
				ID   string
				Read *bool
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Read == nil {
				return nil, badInput("read is required")
			}
			id, err := argID(a.ID)
			if err != nil {
				return nil, err
			}
			if _, err := db.getMail(id); err != nil {
				return nil, badInput("no received mail %s", a.ID)
			}
			if err := db.updateMail(id, func(m *Mail) { m.Read = *a.Read }); err != nil {
				return nil, err
			}
			return jsonResult(map[string]any{"id": idHex(id), "read": *a.Read})
		},
	},
}

func isTextType(t string) bool {
	if strings.HasPrefix(t, "text/") {
		return true
	}
	switch t {
	case "application/json", "application/xml", "application/javascript", "application/x-yaml", "application/yaml", "application/csv":
		return true
	}
	return strings.HasSuffix(t, "+json") || strings.HasSuffix(t, "+xml")
}
