package main

// Persistent state lives in one bbolt file:
//
//	meta     id -> Mail (JSON)      small; what the inbox list reads
//	sent     id -> Mail (JSON)      outgoing mail (compose, reply, forward), same TTL
//	body     id -> Body (JSON)      text and raw HTML, read only when a mail is opened
//	att      id+idx -> bytes        stored attachments, of received and sent mail
//	blocked  pattern -> Rule        sender patterns that are dropped
//	domains  domain -> Rule         recipient domains that are accepted
//	dests    id -> Dest             where accepted mail goes
//	kv       misc flags
//
// A mail id is its arrival (or sending) time in Unix nanoseconds, big-endian,
// unique across meta and sent, so body and att serve both. The keys sort by
// age: expiry is a cursor walk from the start of meta, and the inbox
// is a reverse walk. bbolt never shrinks its file by itself, so the sweeper
// compacts into a fresh file and swaps it in once expired pages pile up.

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bMeta    = []byte("meta")
	bSent    = []byte("sent")
	bBody    = []byte("body")
	bAtt     = []byte("att")
	bBlocked = []byte("blocked")
	bDomains = []byte("domains")
	bDests   = []byte("dests")
	bKV      = []byte("kv")

	errNotFound = errors.New("not found")
)

// FileMeta describes one attachment; Stored says whether its bytes are in att.
type FileMeta struct {
	Name   string `json:"n"`
	Size   int64  `json:"s"`
	Type   string `json:"t,omitempty"`
	Stored bool   `json:"k,omitempty"`
}

// Mail is the inbox row. Everything heavy lives in Body and att.
type Mail struct {
	ID       uint64     `json:"-"`
	At       time.Time  `json:"at"`
	From     string     `json:"f"`
	FromAddr string     `json:"fa"`
	ReplyTo  string     `json:"rt,omitempty"`
	To       string     `json:"t"`
	ToAddr   string     `json:"ta"`
	Cc       string     `json:"cc,omitempty"`
	Subject  string     `json:"s"`
	Date     string     `json:"d"`
	MsgID    string     `json:"mid,omitempty"`
	Refs     string     `json:"refs,omitempty"`
	Files    []FileMeta `json:"files,omitempty"`
	Read     bool       `json:"r,omitempty"`
	Replied  bool       `json:"rp,omitempty"`
	Log      []string   `json:"log,omitempty"` // delivery outcomes, newest last
	Kind     string     `json:"k,omitempty"`   // sent mail only: compose, reply or forward
	Orig     uint64     `json:"o,omitempty"`   // sent mail only: the inbox mail replied to or forwarded
}

// Kinds of sent mail.
const (
	sentCompose = "compose"
	sentReply   = "reply"
	sentForward = "forward"
)

type Body struct {
	Text    string `json:"t,omitempty"`
	HTML    string `json:"h,omitempty"`
	TextCut bool   `json:"tc,omitempty"`
	HTMLCut bool   `json:"hc,omitempty"`
}

// Rule is a blocked sender pattern or an allowed domain.
type Rule struct {
	Key     string    `json:"-"`
	Note    string    `json:"n,omitempty"`
	Created time.Time `json:"c"`
}

// Dest is a Telegram chat or an email address that receives matching mail.
// Senders holds sender patterns; empty means every accepted mail.
type Dest struct {
	ID      uint64   `json:"-"`
	Kind    string   `json:"k"` // "telegram" or "email"
	Target  string   `json:"t"` // chat id, or address
	Name    string   `json:"n,omitempty"`
	Senders []string `json:"s,omitempty"`
	Enabled bool     `json:"e"`
}

const (
	kindTelegram = "telegram"
	kindEmail    = "email"
)

type store struct {
	mu   sync.RWMutex // write-locked only while compaction swaps the file
	db   *bolt.DB
	path string
	last atomic.Uint64 // last issued mail id, keeps ids unique within a nanosecond
}

var (
	db    *store
	rules atomic.Pointer[ruleSet]
)

// ruleSet is an in-memory copy of the small config buckets, so the SMTP path
// never touches the DB just to decide whether to accept a mail.
type ruleSet struct {
	domains map[string]bool
	blocked []string
	dests   []Dest
}

func boltOptions() *bolt.Options {
	return &bolt.Options{
		Timeout:        2 * time.Second,
		FreelistType:   bolt.FreelistMapType,
		NoFreelistSync: true, // rebuilt on open; saves a write per commit
	}
}

func openStore(path string) (*store, error) {
	b, err := bolt.Open(path, 0o600, boltOptions())
	if err != nil {
		return nil, err
	}
	s := &store{db: b, path: path}
	err = b.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bMeta, bSent, bBody, bAtt, bBlocked, bDomains, bDests, bKV} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) view(fn func(*bolt.Tx) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.View(fn)
}

func (s *store) update(fn func(*bolt.Tx) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.Update(fn)
}

// ------------------------------------------------------------------ ids

func (s *store) nextID() uint64 {
	for {
		now := uint64(time.Now().UnixNano())
		last := s.last.Load()
		if now <= last {
			now = last + 1
		}
		if s.last.CompareAndSwap(last, now) {
			return now
		}
	}
}

func idKey(id uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, id)
	return k
}

func attKey(id uint64, idx int) []byte {
	k := make([]byte, 10)
	binary.BigEndian.PutUint64(k, id)
	binary.BigEndian.PutUint16(k[8:], uint16(idx))
	return k
}

func idHex(id uint64) string { return fmt.Sprintf("%016x", id) }

func parseID(s string) (uint64, bool) {
	if len(s) != 16 {
		return 0, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return 0, false
	}
	return binary.BigEndian.Uint64(b), true
}

// ---------------------------------------------------------------- mails

// saveMail writes a new mail with its body and the attachments whose bytes are
// given (nil entries are listed in m.Files but not stored).
func (s *store) saveMail(m *Mail, body *Body, files [][]byte) error {
	return s.saveRow(bMeta, m, body, files)
}

// saveRow is saveMail for either the inbox (meta) or the sent bucket.
func (s *store) saveRow(bucket []byte, m *Mail, body *Body, files [][]byte) error {
	m.ID = s.nextID()
	mj, err := json.Marshal(m)
	if err != nil {
		return err
	}
	bj, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return s.update(func(tx *bolt.Tx) error {
		k := idKey(m.ID)
		if err := tx.Bucket(bucket).Put(k, mj); err != nil {
			return err
		}
		if err := tx.Bucket(bBody).Put(k, bj); err != nil {
			return err
		}
		att := tx.Bucket(bAtt)
		for i, f := range files {
			if f != nil {
				if err := att.Put(attKey(m.ID, i), f); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *store) getMail(id uint64) (*Mail, error) { return s.getRow(bMeta, id) }

func (s *store) getRow(bucket []byte, id uint64) (*Mail, error) {
	var m *Mail
	err := s.view(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get(idKey(id))
		if v == nil {
			return errNotFound
		}
		m = &Mail{ID: id}
		return json.Unmarshal(v, m)
	})
	if err == nil && expired(m) {
		return nil, errNotFound
	}
	return m, err
}

func (s *store) getBody(id uint64) (*Body, error) {
	var b Body
	err := s.view(func(tx *bolt.Tx) error {
		v := tx.Bucket(bBody).Get(idKey(id))
		if v == nil {
			return errNotFound
		}
		return json.Unmarshal(v, &b)
	})
	return &b, err
}

// getAttachment copies one stored attachment out of the DB, so the caller can
// use it after the read transaction has ended.
func (s *store) getAttachment(id uint64, idx int) ([]byte, error) {
	var out []byte
	err := s.view(func(tx *bolt.Tx) error {
		v := tx.Bucket(bAtt).Get(attKey(id, idx))
		if v == nil {
			return errNotFound
		}
		out = bytes.Clone(v)
		return nil
	})
	return out, err
}

// writeAttachment streams one stored attachment straight from the mmap,
// with no heap copy. The read transaction stays open while w drains.
func (s *store) writeAttachment(id uint64, idx int, w func(size int) io.Writer) error {
	return s.view(func(tx *bolt.Tx) error {
		v := tx.Bucket(bAtt).Get(attKey(id, idx))
		if v == nil {
			return errNotFound
		}
		_, err := w(len(v)).Write(v)
		return err
	})
}

// findMail looks an id up in the inbox, then in sent, and names the bucket.
func (s *store) findMail(id uint64) (*Mail, []byte, error) {
	for _, b := range [][]byte{bMeta, bSent} {
		if m, err := s.getRow(b, id); err == nil {
			return m, b, nil
		}
	}
	return nil, nil, errNotFound
}

// updateMail applies fn to a stored row; a row deleted meanwhile is skipped.
func (s *store) updateMail(id uint64, fn func(*Mail)) error { return s.updateRow(bMeta, id, fn) }

func (s *store) updateRow(bucket []byte, id uint64, fn func(*Mail)) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		k := idKey(id)
		v := b.Get(k)
		if v == nil {
			return errNotFound
		}
		m := &Mail{ID: id}
		if err := json.Unmarshal(v, m); err != nil {
			return err
		}
		fn(m)
		mj, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return b.Put(k, mj)
	})
}

// appendLog records one delivery outcome on a mail.
func (s *store) appendLog(id uint64, line string) { s.appendRowLog(bMeta, id, line) }

func (s *store) appendRowLog(bucket []byte, id uint64, line string) {
	stamp := time.Now().Format("Jan 2 15:04")
	if err := s.updateRow(bucket, id, func(m *Mail) {
		m.Log = append(m.Log, stamp+" "+line)
		if len(m.Log) > 20 {
			m.Log = m.Log[len(m.Log)-20:]
		}
	}); err != nil && !errors.Is(err, errNotFound) {
		log.Printf("store: log %s: %v", idHex(id), err)
	}
}

func (s *store) deleteMail(id uint64) error { return s.deleteRow(bMeta, id) }

func (s *store) deleteRow(bucket []byte, id uint64) error {
	return s.update(func(tx *bolt.Tx) error {
		return deleteRowTx(tx, bucket, idKey(id))
	})
}

func deleteRowTx(tx *bolt.Tx, bucket, k []byte) error {
	if err := tx.Bucket(bucket).Delete(k); err != nil {
		return err
	}
	if err := tx.Bucket(bBody).Delete(k); err != nil {
		return err
	}
	c := tx.Bucket(bAtt).Cursor()
	for ak, _ := c.Seek(k); ak != nil && bytes.HasPrefix(ak, k); ak, _ = c.Seek(k) {
		if err := c.Delete(); err != nil {
			return err
		}
	}
	return nil
}

// listMails returns up to limit rows older than before (0: newest first),
// keeping those that contain q in sender, recipient or subject.
func (s *store) listMails(before uint64, q string, limit int) (rows []Mail, more bool, err error) {
	return s.listRows(bMeta, before, q, limit)
}

func (s *store) listRows(bucket []byte, before uint64, q string, limit int) (rows []Mail, more bool, err error) {
	q = strings.ToLower(strings.TrimSpace(q))
	cutoff := uint64(time.Now().Add(-mailTTL).UnixNano())
	err = s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucket).Cursor()
		var k, v []byte
		if before == 0 {
			k, v = c.Last()
		} else {
			c.Seek(idKey(before))
			k, v = c.Prev()
		}
		for ; k != nil; k, v = c.Prev() {
			id := binary.BigEndian.Uint64(k)
			if id < cutoff {
				break
			}
			var m Mail
			if json.Unmarshal(v, &m) != nil {
				continue
			}
			m.ID = id
			if q != "" && !strings.Contains(strings.ToLower(m.From+" "+m.To+" "+m.Subject), q) {
				continue
			}
			if len(rows) == limit {
				more = true
				break
			}
			rows = append(rows, m)
		}
		return nil
	})
	return rows, more, err
}

func (s *store) unreadCount() (n int) {
	cutoff := uint64(time.Now().Add(-mailTTL).UnixNano())
	s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(bMeta).Cursor()
		for k, v := c.Seek(idKey(cutoff)); k != nil; k, v = c.Next() {
			if !bytes.Contains(v, []byte(`"r":true`)) {
				n++
			}
		}
		return nil
	})
	return n
}

func expired(m *Mail) bool { return time.Since(m.At) > mailTTL }

// --------------------------------------------------------------- expiry

// sweep deletes every received and sent mail older than the TTL.
func (s *store) sweep() (removed int, err error) {
	for _, b := range [][]byte{bMeta, bSent} {
		n, err := s.sweepBucket(b)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// sweepBucket deletes a bounded batch per transaction, so a large backlog
// never holds the write lock for long.
func (s *store) sweepBucket(bucket []byte) (removed int, err error) {
	cutoff := idKey(uint64(time.Now().Add(-mailTTL).UnixNano()))
	for {
		n := 0
		err = s.update(func(tx *bolt.Tx) error {
			c := tx.Bucket(bucket).Cursor()
			for k, _ := c.First(); k != nil && bytes.Compare(k, cutoff) < 0 && n < 500; k, _ = c.First() {
				if err := deleteRowTx(tx, bucket, bytes.Clone(k)); err != nil {
					return err
				}
				n++
			}
			return nil
		})
		removed += n
		if err != nil || n < 500 {
			return removed, err
		}
	}
}

// freeBytes is the space inside the file that holds nothing live.
func (s *store) freeBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.db.Stats()
	return int64(st.FreePageN+st.PendingPageN) * int64(s.db.Info().PageSize)
}

// compact rewrites the live data into a new file and swaps it in, returning
// the space held by expired and deleted mail to the filesystem. Writers wait
// for the swap; it takes well under a second for a week of mail.
func (s *store) compact() error {
	tmp := s.path + ".compact"
	os.Remove(tmp)

	s.mu.Lock()
	defer s.mu.Unlock()

	before := fileSize(s.path)
	dst, err := bolt.Open(tmp, 0o600, boltOptions())
	if err != nil {
		return err
	}
	if err := bolt.Compact(dst, s.db, 4<<20); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := s.db.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
	}
	// Reopen whichever file is now in place; failing here is fatal, since
	// every later request would hit a closed handle.
	b, err := bolt.Open(s.path, 0o600, boltOptions())
	if err != nil {
		log.Fatalf("store: reopen after compaction: %v", err)
	}
	s.db = b
	log.Printf("store: compacted %d -> %d bytes", before, fileSize(s.path))
	return nil
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// expiryLoop sweeps hourly. Space freed inside the file is returned to the
// disk right away once it passes 4 MB, and every night at 03:30 otherwise.
func (s *store) expiryLoop() {
	lastNightly := -1
	run := func() {
		n, err := s.sweep()
		if err != nil {
			log.Printf("store: sweep: %v", err)
		} else if n > 0 {
			log.Printf("store: expired %d mail(s)", n)
		}
		free := s.freeBytes()
		now := time.Now()
		nightly := now.Hour() == 3 && now.YearDay() != lastNightly
		if free > 4<<20 || (nightly && free > 0) {
			if nightly {
				lastNightly = now.YearDay()
			}
			if err := s.compact(); err != nil {
				log.Printf("store: compact: %v", err)
			}
		}
	}
	run()
	for {
		// Wake at :30 past each hour, so the nightly run lands at 03:30.
		now := time.Now()
		next := now.Truncate(time.Hour).Add(30 * time.Minute)
		if !next.After(now) {
			next = next.Add(time.Hour)
		}
		time.Sleep(time.Until(next))
		run()
	}
}

// ---------------------------------------------------------------- rules

func (s *store) listRules(bucket []byte) ([]Rule, error) {
	var out []Rule
	err := s.view(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(k, v []byte) error {
			r := Rule{Key: string(k)}
			json.Unmarshal(v, &r)
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (s *store) putRule(bucket []byte, r Rule) error {
	if r.Created.IsZero() {
		r.Created = time.Now()
	}
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	err = s.update(func(tx *bolt.Tx) error { return tx.Bucket(bucket).Put([]byte(r.Key), v) })
	if err == nil {
		s.reloadRules()
	}
	return err
}

func (s *store) deleteRule(bucket []byte, key string) error {
	err := s.update(func(tx *bolt.Tx) error { return tx.Bucket(bucket).Delete([]byte(key)) })
	if err == nil {
		s.reloadRules()
	}
	return err
}

// ---------------------------------------------------------- destinations

func (s *store) listDests() ([]Dest, error) {
	var out []Dest
	err := s.view(func(tx *bolt.Tx) error {
		return tx.Bucket(bDests).ForEach(func(k, v []byte) error {
			d := Dest{ID: binary.BigEndian.Uint64(k)}
			if json.Unmarshal(v, &d) == nil {
				out = append(out, d)
			}
			return nil
		})
	})
	return out, err
}

func (s *store) getDest(id uint64) (*Dest, error) {
	var d *Dest
	err := s.view(func(tx *bolt.Tx) error {
		v := tx.Bucket(bDests).Get(idKey(id))
		if v == nil {
			return errNotFound
		}
		d = &Dest{ID: id}
		return json.Unmarshal(v, d)
	})
	return d, err
}

// putDest creates the destination when d.ID is 0, otherwise replaces it.
func (s *store) putDest(d *Dest) error {
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bDests)
		if d.ID == 0 {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			d.ID = seq
		}
		v, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return b.Put(idKey(d.ID), v)
	})
	if err == nil {
		s.reloadRules()
	}
	return err
}

func (s *store) deleteDest(id uint64) error {
	err := s.update(func(tx *bolt.Tx) error { return tx.Bucket(bDests).Delete(idKey(id)) })
	if err == nil {
		s.reloadRules()
	}
	return err
}

// reloadRules refreshes the in-memory copy after any config change.
func (s *store) reloadRules() {
	rs := &ruleSet{domains: map[string]bool{}}
	if list, err := s.listRules(bDomains); err == nil {
		for _, r := range list {
			rs.domains[r.Key] = true
		}
	}
	if list, err := s.listRules(bBlocked); err == nil {
		for _, r := range list {
			rs.blocked = append(rs.blocked, r.Key)
		}
	}
	if list, err := s.listDests(); err == nil {
		rs.dests = list
	}
	rules.Store(rs)
}

// seed copies the legacy VALID_DOMAINS and TELEGRAM_CHAT_ID settings into the
// DB on first start, so upgrading keeps the relay working unchanged. Later
// edits happen in the web UI; the env keys are then ignored.
func (s *store) seed(domains []string, chat string) error {
	var done bool
	s.view(func(tx *bolt.Tx) error {
		done = tx.Bucket(bKV).Get([]byte("seeded")) != nil
		return nil
	})
	if done {
		return nil
	}
	for _, d := range domains {
		if err := s.putRule(bDomains, Rule{Key: d, Note: "from VALID_DOMAINS"}); err != nil {
			return err
		}
	}
	if chat != "" {
		if err := s.putDest(&Dest{Kind: kindTelegram, Target: chat, Name: "default", Enabled: true}); err != nil {
			return err
		}
	}
	log.Printf("store: seeded %d domain(s) and %v chat from env", len(domains), chat != "")
	return s.update(func(tx *bolt.Tx) error { return tx.Bucket(bKV).Put([]byte("seeded"), []byte("1")) })
}
