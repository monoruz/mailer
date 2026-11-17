# mailer

A small SMTP server that turns incoming email into Telegram notifications, with a web
inbox.

It listens for SMTP, accepts mail addressed to a whitelist of domains, keeps it for a
week in an embedded bbolt database, and sends it to every matching destination: a
Telegram chat (formatted summary, the full email as a sanitized `.html` file when it
is long, attachments as documents) or an email address (forwarded). The web UI lists
the inbox, shows each mail, composes new mail, replies, forwards, keeps a Sent folder
and edits all the rules. An [MCP](#mcp-server) endpoint gives AI agents the same inbox
and sending tools.

The only dependency outside the Go standard library is
[bbolt](https://github.com/etcd-io/bbolt). The UI is server-rendered HTML driven by
[htmx](https://htmx.org), which is embedded in the binary: no CDN, no npm, no build
step. A stripped build is ~9 MB, idles around 15 MB RSS, and its physical footprint
stays near 10 MB after handling large attachments.

## Requirements

Go 1.25 or newer to build. Nothing at runtime — the binary is statically linked.

## Quick start

```sh
make                 # static linux/amd64 binary -> ./mailer
cp .env.example .env # then fill in the token, chat id and domains
chmod 600 .env
./mailer
```

Then open <http://127.0.0.1:8025>. On first start, `VALID_DOMAINS` and
`TELEGRAM_CHAT_ID` are copied into the database as the first allowed domains and
the first destination; after that, the web UI is where they are edited, and those
two keys are ignored.

Send a test mail to the running port and the chat receives:

```
✉️ Invoice for September

👤 Alice <alice@sender.org>
📥 bob@example.com
🕒 7 Oct 2026, 14:03 UTC

┃ Hi, invoice attached.        <- expandable quote
┃ Thanks, Alice

📎 1 file · invoice.pdf
```

followed by `invoice.pdf` as a document, sent as a silent reply to the summary.

A body longer than the message can hold is clipped with `…`, the summary ends with
`📄 Full email attached below`, and `Invoice for September.html` follows as a reply.

## Configuration

Settings are read from a `.env` file when one exists, falling back to the process
environment per key.

| Key | Default | Meaning |
| --- | --- | --- |
| `RADAR_BOT_TOKEN` | — | Telegram bot token. Required for Telegram destinations. |
| `TELEGRAM_CHAT_ID` | — | First Telegram destination. **Seed only:** read on the first start. |
| `VALID_DOMAINS` | empty | Comma-separated recipient domains. **Seed only:** read on the first start. |
| `SMTP_PORT` | `25` | Listen port. |
| `WEB_ADDR` | `127.0.0.1:8025` | Web UI listen address. |
| `WEB_PUBLIC_URL` | — | Public URL of the UI, e.g. `https://mail.example.com`. Adds a "🔗 Open in inbox" link to chat messages. |
| `DB_PATH` | `mailer.db` | The bbolt file, relative to the working directory. The unit does not set it, so the DB is `/opt/mailer/mailer.db`, next to the binary. |
| `MAIL_TTL` | `168h` | How long mail is kept. |
| `STORE_MAX_BYTES` | `10485760` (10 MB) | Attachment bytes stored per mail; files past it are listed but not kept. `0` stores none. |
| `MAIL_HOSTNAME` | hostname | HELO name for outbound mail. Use the FQDN the server's reverse DNS points to. |
| `FORWARD_FROM` | recipient | Sender address of forwarded mail. Default: the address the mail was sent to. |
| `SEND_ATTACHMENTS` | `1` | `0` counts attachments without uploading them. |
| `MAX_ATTACHMENT_BYTES` | `20971520` (20 MB) | Per-file cap. Telegram's own bot upload ceiling is ~50 MB. |
| `TZ` | system | Time zone for the date line, e.g. `Asia/Tehran`. Unlike Go's own `TZ` handling, also read from `.env`. |
| `DEBUG` | off | `1` logs every MIME part decision and Telegram reply. |
| `TELEGRAM_API_BASE` | `https://api.telegram.org` | API override, for testing against a local stub. |
| `ENV_FILE` | — | Explicit path to the config file. Read from the environment only. |

### How the config file is found

First match wins:

1. `$ENV_FILE`
2. `./.env` (working directory)
3. `.env` next to the binary — this is what saves you when a service runs with `cwd=/`

Precedence is **per key**: a key present in `.env` beats the environment, and a key
absent from `.env` falls back to it. So secrets can live in the file while the port
is overridden from a shell.

The parser accepts `#` comments, blank lines, an `export ` prefix, single and double
quotes (`\n`, `\"`, `\\` inside double quotes) and a trailing ` #` comment on
unquoted values. Malformed lines are skipped rather than aborting startup. The file
is read once at boot, capped at 64 KB. Startup logs the path and the number of keys,
never the values, and warns if the file is group- or world-readable.

## Deployment

The unit file in [`mailer.service`](mailer.service) runs the relay as an unprivileged
user while still binding port 25, via `AmbientCapabilities=CAP_NET_BIND_SERVICE`.

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin mailer
sudo install -d -o mailer -g mailer /opt/mailer
sudo install -o mailer -g mailer -m 755 mailer /opt/mailer/mailer
sudo install -o mailer -g mailer -m 600 .env /opt/mailer/.env
sudo cp mailer.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mailer
journalctl -u mailer -f
```

`WorkingDirectory=/opt/mailer` is what lets the service find `.env`. The unit sets
`ProtectSystem=strict` and `PrivateTmp=true`: attachment spools above 512 KB go to
a private `/tmp`. The DB, `mailer.db`, lives in `/opt/mailer` beside the binary,
together with `mailer.db.compact` while the nightly compaction runs.

Expose the UI with a Cloudflare tunnel, for example:

```sh
cloudflared tunnel route dns mailer mail.example.com
# ingress in ~/.cloudflared/config.yml:
#   - hostname: mail.example.com
#     service: http://127.0.0.1:8025
```

and protect `mail.example.com` with a Zero Trust Access application.

## Web UI

| Page | What it does |
| --- | --- |
| **Inbox** | Newest first, 50 per page, search by sender/recipient/subject, refreshes every 30 s. Unread mail is bold; the nav shows the unread count. |
| **Mail** | Headers, time to expiry, the delivery log (each Telegram send and forward, with errors and retries), stored attachments to download, the body. HTML mail renders in a sandboxed iframe with scripts and every remote load blocked. Reply and Forward forms. Delete. |
| **✎ Compose** | New plain-text mail to one recipient, from any address at an allowed domain (local part + domain picker; default `FORWARD_FROM`, else `mailer@<first domain>`), with up to 10 attachments totalling `STORE_MAX_BYTES`. Sent synchronously; a failure shows the error and keeps the form. |
| **Sent** | Every composed mail, reply and forward, newest first, searchable, kept for `MAIL_TTL`. Shows the body, attachments, a link to the original and the delivery log (a forward's retries show here too). Delete. |
| **Destinations** | Add, edit, enable/disable, test and delete Telegram chats and forward addresses, each with an optional sender filter. |
| **Allowed domains** | Recipient domains that are accepted. None means every mail is dropped. |
| **Blocked senders** | Sender patterns whose mail is dropped and never stored. |

There is **no login**. The UI binds to `127.0.0.1:8025` and is meant to sit behind
Cloudflare Zero Trust (a `cloudflared` tunnel to `http://127.0.0.1:8025`) or another
authenticating proxy. State-changing requests must carry htmx's `HX-Request` header
and must not be cross-site fetches, which blocks CSRF through the proxy's cookie.

### Sender patterns

Used by blocked senders and destination filters, case-insensitive:

| Pattern | Matches |
| --- | --- |
| `alice@example.com` | that address |
| `example.com` | that domain and its subdomains |
| `*@example.com` | glob: `*` any run, `?` one character, `[a-z]` a class; not subdomains |
| `*.gov` | glob over the whole address |

The sender is the `From:` header address, or the envelope `MAIL FROM` when there is
none. Both can be forged, so treat filters as routing, not as security.

### Routing

1. Recipient domain not allowed → dropped.
2. Sender blocked → dropped.
3. Stored, then sent to every **enabled** destination whose filter matches. An empty
   filter matches everything. No match → stored in the inbox only.

Telegram sends finish before the SMTP `250`, as before. Email forwards are queued and
delivered in the background with retries after 2 min, 10 min, 45 min and 3 h; a 5xx
answer stops retrying. Retries pending at restart are lost (the delivery log shows
the last state).

### Outbound mail (compose, replies and forwards)

Delivered straight to the recipient's MX on port 25, with opportunistic STARTTLS and
no DKIM signature. A reply is sent **from** the address the mail was sent to, threaded
with `In-Reply-To`/`References`, and quotes the original. A forward is sent from the
same address (or `FORWARD_FROM`), with `Reply-To` set to the original sender and the
stored attachments included.

For this to reach inboxes:

- outbound port 25 must be open (many clouds block it by default);
- publish an SPF record for your domain that includes this server's IP, e.g.
  `v=spf1 ip4:203.0.113.7 -all`;
- give the IP a reverse DNS name and set `MAIL_HOSTNAME` to it.

Without DKIM, Gmail and Outlook may still put it in spam.

## Storage

One bbolt file. Mail ids are arrival times in nanoseconds, so keys sort by age.

| Bucket | Holds |
| --- | --- |
| `meta` | inbox row per mail (headers, file list, read flag, delivery log) |
| `sent` | the same row for sent mail, plus its kind (compose/reply/forward) and the original's id |
| `body` | text and raw HTML (up to 256 KB / 512 KB), read only when a mail is opened; received and sent |
| `att` | stored attachment bytes, received and composed (a forward's attachments stay with the original) |
| `domains`, `blocked`, `dests` | settings, mirrored in memory so SMTP never reads the DB to route |

**Expiry:** every hour at :30, received and sent mail older than `MAIL_TTL` is deleted. The inbox
hides it the moment it passes the TTL, even before the sweep.

**Disk:** bbolt reuses freed pages but never shrinks its file. Once freed space
exceeds 4 MB, and every night at 03:30 otherwise, the file is compacted into a fresh
copy and swapped in, so expired mail leaves no disk behind. Writers wait during the
swap, typically well under a second; it briefly needs space for both files.

**Memory:** the file is memory-mapped, so reads cost page cache rather than heap.
Attachment downloads stream straight from the map. After a mail with more than 1 MB
of attachments, freed heap is returned to the OS right away.

## MCP server

`POST /mcp` on the web port speaks the [Model Context Protocol](https://modelcontextprotocol.io)
over Streamable HTTP (JSON responses, stateless, protocol versions 2024-11-05 to
2025-11-25), with no extra dependency.

| Tool | Does |
| --- | --- |
| `list_mails` | inbox or sent, newest first; `query` searches sender/recipient/subject; paged with `before` |
| `get_mail` | headers, plain-text body, attachment list, delivery log (does not mark read) |
| `get_attachment` | one stored attachment, ≤ 5 MB: text inline, images as image content, else a base64 blob |
| `send_mail` | new mail; `from` must be at an allowed domain; attachments as base64 |
| `reply_mail` | quoted, threaded reply to an inbox mail |
| `forward_mail` | queued forward with attachments; the returned sent id shows the outcome |
| `delete_mail` | received or sent mail |
| `mark_mail` | read / unread |

**`/mcp` has no authentication of its own.** Protect it in Cloudflare Access with a
self-hosted application for `mail.example.com/mcp` and a **Service Auth** policy, then
create a service token and have the client send it:

```json
{
  "mcpServers": {
    "mailer": {
      "type": "http",
      "url": "https://mail.example.com/mcp",
      "headers": {
        "CF-Access-Client-Id": "<id>.access",
        "CF-Access-Client-Secret": "<secret>"
      }
    }
  }
}
```

A **Bypass** policy instead leaves `/mcp` open to the internet: anyone who finds the
URL can read every mail and send mail from your domains, which makes the server a
spam source and burns the domains' reputation.

Browsers cannot drive it from another site: the body must be `application/json`
(forcing a CORS preflight that is never answered) and a foreign `Origin` is refused.
`GET` and `DELETE` answer 405, since there is no SSE stream or session.

## Operation

Every accepted mail logs one line:

```
mail id=18e440989c952a00 from=alice@sender.org to=bob@example.com attachments=2 destinations=1 full=false
```

`full=true` means the body did not fit and the `.html` file was sent too. Per
destination outcomes are in the mail's delivery log in the UI; failures are also
logged. Dropped mail logs instead:

```
Rejected: destination domain "spam.io" not whitelisted
Blocked: sender "x@spam.io"
```

The store logs its housekeeping:

```
store: expired 12 mail(s)
store: compacted 34897920 -> 131072 bytes
```

To test the Telegram side alone, with no mail involved:

```sh
./mailer -test-upload ./somefile.pdf
```

It uploads that one file to the configured chat and prints the API's verbatim reply,
which separates bot/chat problems from SMTP or MIME problems.

### Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| No `config:` line at startup | An older binary is running. Rebuild and restart. |
| `Rejected: destination domain ... not whitelisted` | Add the domain under **Allowed domains**. An empty domain means the mail carries no `To:` header and the envelope recipient is empty. |
| Mail in the inbox but not in Telegram | No enabled destination matches the sender, or the send failed: see the mail's delivery log. |
| Reply or forward fails with a timeout | Outbound port 25 is blocked by the host or cloud provider. |
| `store: open ...: timeout` | Another process holds the DB file open. |
| Summary arrives, no documents | Check the log for `sendDocument` errors; `SEND_ATTACHMENTS=0` also produces this. |
| `bind: permission denied` on port 25 | Missing `CAP_NET_BIND_SERVICE`, or running outside the unit. |
| Nothing at all | Mail never reached the port. Check DNS/MX and firewall. |

## How it works

### SMTP

A hand-written subset of RFC 5321: `HELO`, `EHLO`, `MAIL`, `RCPT`, `DATA`, `RSET`,
`NOOP`, `VRFY`, `QUIT`. `STARTTLS` and `AUTH` answer `502` — there is no TLS and no
authentication, matching the original service.

Mail for a non-whitelisted domain is accepted with `250` and silently dropped, so
senders are not told which domains exist. A parse failure answers `451`, letting the
sender retry. Each connection runs in its own goroutine with a panic barrier, a
5-minute command deadline and a 10-minute `DATA` deadline.

### Recipient whitelist

The domain of the first address in the `To:` header is matched, case-insensitively,
against `VALID_DOMAINS`. When there is no `To:` header — Bcc-style delivery — the
envelope `RCPT TO` address is used instead.

### Parsing

The message is streamed, never buffered whole. `net/mail` reads only the headers,
then the MIME tree is walked with `mime/multipart`, at most 8 levels deep:

- The **body** is the first `text/plain` part. With no plain-text part, the HTML is
  flattened to text, keeping the line breaks its block elements imply and dropping
  the invisible padding marketing mail puts in its preheader.
- Every other leaf — anything with `Content-Disposition: attachment`, a `filename`
  or `name` parameter, or a non-`text/*` type — counts as an attachment.
- Attachment parts that are not forwarded are discarded without being read, so a
  20 MB PDF costs nothing when `SEND_ATTACHMENTS=0`.

Headers are RFC 2047 decoded, and `quoted-printable`, `base64`, UTF-8, US-ASCII and
latin-1/cp1252 are handled without any external charset tables. Base64 missing its
`=` padding — which some mailers emit — is repaired rather than rejected.

### Attachments

Each forwarded attachment is spooled while `DATA` is read: in memory up to 512 KB,
otherwise into a temp file that is unlinked immediately after creation, so the space
is reclaimed on close and nothing survives a crash. The upload streams straight from
the spool with an exact `Content-Length`, so the file is never copied into a second
buffer.

The summary message is sent first, then the full email when needed, then one
`sendDocument` per file. Everything after the summary is a silent reply to it, so one
mail makes one notification and its files stay grouped. All of it happens before the
SMTP `250` — which keeps backpressure on the sender rather than acknowledging mail
that has not been delivered to Telegram yet. A file over the cap is listed in the
summary as `(too large)` and skipped.

### Full email file

Sent when the body is longer than the space left in the summary (at most 3000
characters), or when it was too long to capture whole. It is a standalone page: a
header card (subject, from, to, cc, date, files) over the mail's own HTML, or over the
plain text with its links made clickable when the mail has no HTML part.

The mail's HTML is sanitized before it is written out, since the file is opened on a
phone straight from the chat:

- A `Content-Security-Policy` meta tag blocks scripts, every remote load (images,
  fonts, CSS) and form submission. This is the main barrier.
- Defense in depth, for viewers that ignore CSP: `<script>`, `<iframe>`, `<object>`,
  `<embed>` and similar are removed with their content; `on*` handlers, `srcset`,
  form actions and `javascript:`/`vbscript:`/`data:` URLs are dropped; remote
  `<img src>` is renamed to `data-blocked-src`, so tracking pixels never fire.
- Links open in a new tab with no referrer.

The card notes how many remote images were blocked, and when the mail was longer
than the capture limit.

### Limits

| Limit | Value | Why |
| --- | --- | --- |
| Bytes per connection | 40 MB | A 20 MB attachment is ~27 MB once base64-encoded. |
| Concurrent connections | 64 | |
| Command line length | 1 KB | |
| Body captured | 256 KB plain / 512 KB HTML | Kept whole for the full-email file. |
| Body shown inline | 3000 characters | Less when long headers take the room. |
| Telegram text | 4000 UTF-16 units | Under the API's 4096 limit, counted the way the API counts. |
| Attachments forwarded | 10 per message | Further ones are counted only. |
| Spooled per message | 30 MB | |
| Messages spooling at once | 8 | Caps transient disk at roughly 240 MB. |

Changing any of these means editing the `const` block at the top of `main.go`.

## Security notes

- The web UI has **no login of its own**. Keep `WEB_ADDR` on `127.0.0.1` and reach it
  only through an authenticating proxy such as Cloudflare Access.
- `/mcp` can read all mail and send as any address at an allowed domain. Guard it with
  a Cloudflare Access Service Auth policy, never a Bypass (see [MCP server](#mcp-server)).
- SMTP has **no TLS and no authentication**. Bind it to a private interface, or put
  it behind a firewall that only admits your MX. It is not an open relay — mail is
  never forwarded on — but anyone who can reach the port can post into your chat.
- `.env` holds a bot token. Keep it `chmod 600`; the relay warns when it is not.
- Attachments are forwarded verbatim. Anyone who can send mail to a whitelisted
  domain can put a file in your Telegram chat.

## Make targets

| Target | Result |
| --- | --- |
| `make` | static `linux/amd64` binary |
| `make arm64` | `linux/arm64` -> `mailer-arm64` |
| `make local` | build for the current machine |
| `make run` | build locally and run |
| `make test-upload FILE=x.pdf` | the Telegram upload probe |
| `make check` | fmt, vet, compile |
| `make clean` | remove binaries |

Overridable: `make GOARCH=arm64`, `make BIN=relay`, `make LDFLAGS=` to keep symbols.
