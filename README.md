# mailer

A small SMTP server that turns incoming email into Telegram notifications.

It listens for SMTP, accepts mail addressed to a whitelist of domains, and posts a
formatted summary to a Telegram chat. When the body is too long for one message, the
whole email follows as a sanitized `.html` file; attachments follow as documents. It
never relays mail onward.

Written against the Go standard library only: no modules, no `go.sum`, no vendor
directory. A stripped `linux/amd64` build is ~5.8 MB and idles around 5 MB RSS.

## Requirements

Go 1.25 or newer to build. Nothing at runtime — the binary is statically linked.

## Quick start

```sh
make                 # static linux/amd64 binary -> ./mailer
cp .env.example .env # then fill in the token, chat id and domains
chmod 600 .env
./mailer
```

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
| `RADAR_BOT_TOKEN` | — | Telegram bot token. Required. |
| `TELEGRAM_CHAT_ID` | — | Target chat id. Required. |
| `VALID_DOMAINS` | empty | Comma-separated recipient domains. **Empty means every mail is dropped.** |
| `SMTP_PORT` | `25` | Listen port. |
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
`ProtectSystem=strict` with `PrivateTmp=true`, which gives the process a private
`/tmp` — the only place it writes, and only for attachments above 512 KB.

## Operation

Every accepted mail logs one line:

```
mail to=bob@example.com attachments=2 forwarded=2 full=false
```

`full=true` means the body did not fit and the `.html` file was sent too.

`forwarded=0` alongside a non-zero `attachments` means uploads failed; the reason is
on the preceding line. Rejected recipients log instead:

```
Rejected: destination domain "spam.io" not whitelisted
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
| `Rejected: destination domain "" not whitelisted` | `VALID_DOMAINS` unset, or the mail carries no `To:` header and the envelope recipient is empty. |
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

- There is **no TLS and no authentication**. Bind it to a private interface, or put
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
