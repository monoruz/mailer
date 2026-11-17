package main

import (
	"errors"
	"net/mail"
	"path"
	"regexp"
	"strings"
)

// Sender patterns, shared by blocked senders and destination filters:
//
//	alice@example.com   that address
//	example.com         that domain and its subdomains
//	*@example.com       glob: * any run, ? one character, [a-z] a class
//	*.gov               glob over the whole address
//
// Matching is case-insensitive.

// normPattern lower-cases and validates a sender pattern.
func normPattern(p string) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "", errors.New("pattern is empty")
	}
	if strings.ContainsAny(p, " \t,;<>") {
		return "", errors.New("pattern may not contain spaces, commas or brackets")
	}
	if _, err := path.Match(p, ""); err != nil {
		return "", errors.New("bad glob pattern")
	}
	return p, nil
}

// splitPatterns reads a comma or newline separated list of patterns.
func splitPatterns(s string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if strings.TrimSpace(f) == "" {
			continue
		}
		p, err := normPattern(f)
		if err != nil {
			return nil, errors.New(strings.TrimSpace(f) + ": " + err.Error())
		}
		out = append(out, p)
	}
	return out, nil
}

func matchSender(pattern, addr string) bool {
	addr = strings.ToLower(addr)
	if !strings.ContainsAny(pattern, "*?[") {
		if strings.Contains(pattern, "@") {
			return addr == pattern
		}
		return strings.HasSuffix(addr, "@"+pattern) || strings.HasSuffix(addr, "."+pattern)
	}
	ok, _ := path.Match(pattern, addr)
	return ok
}

func matchAny(patterns []string, addr string) bool {
	for _, p := range patterns {
		if matchSender(p, addr) {
			return true
		}
	}
	return false
}

var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func normDomain(d string) (string, error) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	d = strings.TrimPrefix(d, "@")
	if !domainRE.MatchString(d) {
		return "", errors.New("not a valid domain")
	}
	return d, nil
}

var chatRE = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z][A-Za-z0-9_]{3,31})$`)

// normDest validates a destination's kind and target.
func normDest(kind, target string) (string, error) {
	target = strings.TrimSpace(target)
	switch kind {
	case kindTelegram:
		if !chatRE.MatchString(target) {
			return "", errors.New("chat id must be a number like -1001234567890 or @channelname")
		}
		return target, nil
	case kindEmail:
		a, err := mail.ParseAddress(target)
		if err != nil || !strings.Contains(a.Address, "@") {
			return "", errors.New("not a valid email address")
		}
		return strings.ToLower(a.Address), nil
	}
	return "", errors.New("unknown destination type")
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// destsFor returns the enabled destinations whose filter accepts the sender.
func (rs *ruleSet) destsFor(sender string) []Dest {
	var out []Dest
	for _, d := range rs.dests {
		if d.Enabled && (len(d.Senders) == 0 || matchAny(d.Senders, sender)) {
			out = append(out, d)
		}
	}
	return out
}
