package migrate

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TLS modes of an Endpoint.
const (
	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
	TLSNone     = "none"
)

// Names and FETCH/STATUS items used more than once.
const (
	inboxName = "INBOX"
	itemXGUID = "X-GUID"
	itemUID   = "UID"
	itemSize  = "RFC822.SIZE"
	itemSave  = "SAVEDATE"
)

// Endpoint is how a source service is reached.
type Endpoint struct {
	// Addr is host:port.
	Addr string
	// TLS is "implicit", "starttls" or "none".
	TLS string
	// ServerName verifies the server certificate ("" = host of Addr).
	ServerName string
	// CAFile adds trusted CAs ("" = system roots).
	CAFile string
	// Proxy sends a PROXY protocol v1 header first (backends behind a
	// load balancer that require it).
	Proxy bool
	// Timeout bounds connecting and every command.
	Timeout time.Duration
}

// tlsConfig is the client TLS configuration of the endpoint.
func (e Endpoint) tlsConfig() (*tls.Config, error) {
	name := e.ServerName
	if name == "" {
		host, _, err := net.SplitHostPort(e.Addr)
		if err != nil {
			return nil, err
		}

		name = host
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name}

	if e.CAFile != "" {
		pem, err := os.ReadFile(e.CAFile)
		if err != nil {
			return nil, err
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificate")
		}

		cfg.RootCAs = pool
	}

	return cfg, nil
}

// dial connects to the endpoint, sends the PROXY header if configured and
// wraps implicit TLS.
func (e Endpoint) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: e.Timeout}

	conn, err := d.DialContext(ctx, "tcp", e.Addr)
	if err != nil {
		return nil, err
	}

	if e.Proxy {
		if _, err := conn.Write([]byte(proxyHeader(conn))); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}

	if e.TLS != TLSImplicit {
		return conn, nil
	}

	cfg, err := e.tlsConfig()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	tc := tls.Client(conn, cfg)
	_ = tc.SetDeadline(time.Now().Add(e.Timeout))

	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return tc, nil
}

// proxyHeader is the PROXY v1 header for conn (our own addresses: the
// backend sees the migration client as the peer).
func proxyHeader(conn net.Conn) string {
	src, _ := conn.LocalAddr().(*net.TCPAddr)
	dst, _ := conn.RemoteAddr().(*net.TCPAddr)

	if src == nil || dst == nil {
		return "PROXY UNKNOWN\r\n"
	}

	family := "TCP4"
	if src.IP.To4() == nil || dst.IP.To4() == nil {
		family = "TCP6"
	}

	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n", family, src.IP, dst.IP, src.Port, dst.Port)
}

// imapError is a NO or BAD completion.
type imapError struct {
	status, text string
}

func (e *imapError) Error() string { return e.status + " " + e.text }

// isBAD reports a BAD completion (unsupported syntax or item).
func isBAD(err error) bool {
	var ie *imapError
	return errors.As(err, &ie) && ie.status == "BAD"
}

// imapClient is a minimal IMAP4rev1 client for reading a source account.
type imapClient struct {
	ep   Endpoint
	conn net.Conn
	lr   *lineReader
	tag  int
	caps map[string]bool
	// skippedNamespaces are personal namespaces besides the first,
	// which listMailboxes does not return.
	skippedNamespaces []string
}

// dialIMAP connects and reads the greeting; STARTTLS if configured.
func dialIMAP(ctx context.Context, ep Endpoint) (*imapClient, error) {
	conn, err := ep.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", ep.Addr, err)
	}

	c := &imapClient{ep: ep, conn: conn, lr: &lineReader{br: bufio.NewReader(conn)}}

	_ = conn.SetDeadline(time.Now().Add(ep.Timeout))

	greeting, err := c.lr.readResponse()
	if err != nil || !strings.HasPrefix(greeting.text, "* OK") {
		_ = conn.Close()
		return nil, fmt.Errorf("imap greeting from %s: %v", ep.Addr, err)
	}

	if ep.TLS == TLSStartTLS {
		if _, err := c.cmd("STARTTLS"); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}

		cfg, err := ep.tlsConfig()
		if err != nil {
			_ = conn.Close()
			return nil, err
		}

		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}

		c.conn, c.lr = tc, &lineReader{br: bufio.NewReader(tc)}
	}

	return c, nil
}

// Close closes the connection (after LOGOUT when possible).
func (c *imapClient) Close() {
	_, _ = c.cmd("LOGOUT")
	_ = c.conn.Close()
}

// cmd runs a command and returns its untagged responses.
func (c *imapClient) cmd(command string) ([]*response, error) {
	var out []*response

	err := c.cmdEach(command, func(r *response) error {
		out = append(out, r)
		return nil
	})

	return out, err
}

// cmdEach runs a command and hands every untagged response to fn.
func (c *imapClient) cmdEach(command string, fn func(*response) error) error {
	c.tag++
	tag := "m" + strconv.Itoa(c.tag)

	_ = c.conn.SetDeadline(time.Now().Add(c.ep.Timeout))

	if _, err := c.conn.Write([]byte(tag + " " + command + "\r\n")); err != nil {
		return err
	}

	var ferr error

	for {
		_ = c.conn.SetDeadline(time.Now().Add(c.ep.Timeout))

		r, err := c.lr.readResponse()
		if err != nil {
			return err
		}

		switch {
		case strings.HasPrefix(r.text, "* "):
			if ferr == nil {
				ferr = fn(r)
			}
		case strings.HasPrefix(r.text, "+"):
			return errors.New("unexpected continuation request")
		case strings.HasPrefix(r.text, tag+" "):
			status, text, _ := strings.Cut(r.text[len(tag)+1:], " ")
			if !strings.EqualFold(status, "OK") {
				return &imapError{status: strings.ToUpper(status), text: text}
			}

			return ferr
		}
	}
}

// login authenticates and loads the capabilities.
func (c *imapClient) login(user, password string) error {
	if _, err := c.cmd("LOGIN " + quote(user) + " " + quote(password)); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	rs, err := c.cmd("CAPABILITY")
	if err != nil {
		return err
	}

	c.caps = map[string]bool{}

	for _, r := range rs {
		if f := strings.Fields(r.text); len(f) > 1 && strings.EqualFold(f[1], "CAPABILITY") {
			for _, cp := range f[2:] {
				c.caps[strings.ToUpper(cp)] = true
			}
		}
	}

	return nil
}

// srcMailbox is a mailbox of the source account.
type srcMailbox struct {
	Raw        string // name as the source knows it (for commands)
	Name       string // ShardPost name (UTF-8, "/" hierarchy)
	Selectable bool
	Subscribed bool
	SpecialUse []string
	Err        error // the name cannot be mapped
}

// specialUses are the RFC 6154 attributes.
var specialUses = map[string]string{`\all`: `\All`, `\archive`: `\Archive`, `\drafts`: `\Drafts`, `\flagged`: `\Flagged`,
	`\junk`: `\Junk`, `\sent`: `\Sent`, `\trash`: `\Trash`}

// listMailboxes lists the personal namespace. A name that cannot be
// mapped comes back with Err set (reported for that mailbox, the others
// go on).
func (c *imapClient) listMailboxes() ([]srcMailbox, error) {
	var ns namespaces

	if c.caps["NAMESPACE"] {
		rs, err := c.cmd("NAMESPACE")
		if err != nil {
			return nil, err
		}

		ns = parseNamespaces(rs)
		c.skippedNamespaces = ns.extra
	}

	rs, subscribed, err := c.list(ns.prefix)
	if err != nil {
		return nil, err
	}

	delim := ns.delim

	var out []srcMailbox

	for _, r := range rs {
		e, ok := parseList(r)
		if !ok || ns.foreign(e.raw) {
			continue
		}

		delim = cmp.Or(delim, e.delim)
		mb := srcMailbox{Raw: e.raw, Selectable: true, Subscribed: subscribed[e.raw]}

		if mb.Name, err = mapName(e.raw, ns.prefix, delim); err != nil {
			mb.Name, mb.Err = e.raw, err
		}

		mb.applyAttrs(e.attrs)
		out = append(out, mb)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

// list runs LIST (with RETURN SUBSCRIBED SPECIAL-USE where supported,
// else LIST and LSUB) and returns the LIST responses and the names LSUB
// reported. With a namespace prefix INBOX is listed as well ("INBOX." +
// "*" does not match INBOX itself).
func (c *imapClient) list(prefix string) ([]*response, map[string]bool, error) {
	patterns := []string{quote(prefix + "*")}
	if prefix != "" {
		patterns = append(patterns, quote(inboxName))
	}

	if c.caps["LIST-EXTENDED"] && c.caps["SPECIAL-USE"] {
		rs, err := c.cmd(`LIST "" (` + strings.Join(patterns, " ") + `) RETURN (SUBSCRIBED SPECIAL-USE)`)
		return rs, map[string]bool{}, err
	}

	var rs []*response

	subscribed := map[string]bool{}

	for _, pattern := range patterns {
		list, err := c.cmd(`LIST "" ` + pattern)
		if err != nil {
			return nil, nil, err
		}

		subs, err := c.cmd(`LSUB "" ` + pattern)
		if err != nil {
			return nil, nil, err
		}

		rs = append(rs, list...)

		for _, r := range subs {
			if e, ok := parseList(r); ok {
				subscribed[e.raw] = true
			}
		}
	}

	return rs, subscribed, nil
}

// applyAttrs sets selectability, subscription and special-use from LIST
// attributes.
func (mb *srcMailbox) applyAttrs(attrs []string) {
	for _, a := range attrs {
		la := strings.ToLower(a)

		switch {
		case la == `\noselect` || la == `\nonexistent`:
			mb.Selectable = false
		case la == `\subscribed`:
			mb.Subscribed = true
		case specialUses[la] != "":
			mb.SpecialUse = append(mb.SpecialUse, specialUses[la])
		}
	}
}

// namespaces are the personal namespace (prefix, delimiter) and the
// prefixes of the other users' and shared namespaces.
type namespaces struct {
	prefix, delim string
	others        []string
	// extra are further personal namespaces; they are not imported (a
	// warning names them).
	extra []string
}

// foreign reports whether raw belongs to another namespace (a longer
// prefix than the personal one wins: "" versus "Shared/").
func (ns namespaces) foreign(raw string) bool {
	for _, p := range ns.others {
		if p != "" && len(p) > len(ns.prefix) && strings.HasPrefix(raw, p) {
			return true
		}
	}

	return false
}

// parseNamespaces reads a NAMESPACE response.
func parseNamespaces(rs []*response) namespaces {
	var ns namespaces

	for _, r := range rs {
		vs, err := newParser(r, 2).values()
		if err != nil || len(vs) < 2 {
			continue
		}

		if name, _ := str(vs[0]); !strings.EqualFold(name, "NAMESPACE") {
			continue
		}

		for i, section := range vs[1:] {
			list, _ := section.([]value)

			for j, entry := range list {
				fields, ok := entry.([]value)
				if !ok || len(fields) < 2 {
					continue
				}

				prefix, _ := str(fields[0])
				delim, _ := str(fields[1])

				switch {
				case i == 0 && j == 0:
					ns.prefix, ns.delim = prefix, delim
				case i == 0:
					ns.extra = append(ns.extra, prefix)
					ns.others = append(ns.others, prefix)
				default:
					ns.others = append(ns.others, prefix)
				}
			}
		}
	}

	return ns
}

type listEntry struct {
	attrs []string
	delim string
	raw   string
}

// parseList parses "* LIST (attrs) delim name" (or LSUB).
func parseList(r *response) (listEntry, bool) {
	vs, err := newParser(r, 2).values()
	if err != nil || len(vs) < 4 {
		return listEntry{}, false
	}

	kind, _ := str(vs[0])
	if !strings.EqualFold(kind, "LIST") && !strings.EqualFold(kind, "LSUB") {
		return listEntry{}, false
	}

	var e listEntry

	if attrs, ok := vs[1].([]value); ok {
		for _, a := range attrs {
			if s, ok := str(a); ok {
				e.attrs = append(e.attrs, s)
			}
		}
	}

	e.delim, _ = str(vs[2])

	raw, ok := str(vs[3])
	if !ok {
		return listEntry{}, false
	}

	e.raw = raw

	return e, true
}

// mapName turns a source name into a ShardPost name: without the
// namespace prefix, decoded from modified UTF-7, "/" as the hierarchy
// delimiter, INBOX canonical.
func mapName(raw, prefix, delim string) (string, error) {
	if strings.EqualFold(raw, inboxName) {
		return inboxName, nil
	}

	name, err := decodeMUTF7(strings.TrimPrefix(raw, prefix))
	if err != nil {
		return "", err
	}

	if delim != "" && delim != "/" {
		if strings.Contains(name, "/") {
			return "", errors.New(`the name contains "/", the hierarchy delimiter here`)
		}

		name = strings.ReplaceAll(name, delim, "/")
	}

	if len(name) >= 6 && strings.EqualFold(name[:6], "INBOX/") {
		name = inboxName + name[5:]
	}

	return name, nil
}

// srcStatus is the identity state of a source mailbox.
type srcStatus struct {
	UIDValidity   uint32
	UIDNext       uint32
	HighestModSeq uint64
	Messages      uint32
	GUID          string
}

// status reads the identity state (X-GUID where the server knows it).
func (c *imapClient) status(raw string) (*srcStatus, error) {
	items := "MESSAGES UIDNEXT UIDVALIDITY"
	if c.caps["CONDSTORE"] {
		items += " HIGHESTMODSEQ"
	}

	withGUID := !c.caps["X-NO-GUID"]

	rs, err := c.cmd("STATUS " + quote(raw) + " (" + items + guidItem(withGUID) + ")")
	if withGUID && isBAD(err) {
		c.caps["X-NO-GUID"] = true
		rs, err = c.cmd("STATUS " + quote(raw) + " (" + items + ")")
	}

	if err != nil {
		return nil, err
	}

	st := parseStatus(rs)
	if st.UIDValidity == 0 || st.UIDNext == 0 {
		return nil, fmt.Errorf("status %q: no UIDVALIDITY/UIDNEXT", raw)
	}

	return st, nil
}

// parseStatus reads the STATUS response.
func parseStatus(rs []*response) *srcStatus {
	st := &srcStatus{}

	for _, r := range rs {
		vs, err := newParser(r, 2).values()
		if err != nil || len(vs) < 3 {
			continue
		}

		if kind, _ := str(vs[0]); !strings.EqualFold(kind, "STATUS") {
			continue
		}

		kv, _ := vs[2].([]value)
		for i := 0; i+1 < len(kv); i += 2 {
			k, _ := str(kv[i])
			st.set(strings.ToUpper(k), kv[i+1])
		}
	}

	return st
}

// set stores one STATUS item.
func (st *srcStatus) set(k string, v value) {
	n, _ := num(v)

	switch k {
	case "MESSAGES":
		st.Messages = uint32(n)
	case "UIDNEXT":
		st.UIDNext = uint32(n)
	case "UIDVALIDITY":
		st.UIDValidity = uint32(n)
	case "HIGHESTMODSEQ":
		st.HighestModSeq = n
	case itemXGUID:
		st.GUID, _ = str(v)
	}
}

func guidItem(on bool) string {
	if on {
		return " " + itemXGUID
	}

	return ""
}

// srcMessage is the identity of a source message.
type srcMessage struct {
	UID          uint32
	Flags        []string
	ModSeq       uint64
	InternalDate time.Time
	SaveDate     time.Time
	Size         int64
	GUID         string
}

// imapDate is the INTERNALDATE/SAVEDATE layout.
const imapDate = "_2-Jan-2006 15:04:05 -0700"

// examine selects the mailbox read-only.
func (c *imapClient) examine(raw string) error {
	_, err := c.cmd("EXAMINE " + quote(raw))
	return err
}

// fetchMeta reads the identity of every message of the examined mailbox.
func (c *imapClient) fetchMeta() ([]srcMessage, error) {
	items := "UID FLAGS INTERNALDATE RFC822.SIZE"
	if c.caps["CONDSTORE"] {
		items += " MODSEQ"
	}

	if c.caps["SAVEDATE"] {
		items += " SAVEDATE"
	}

	withGUID := !c.caps["X-NO-GUID"]

	msgs, err := c.fetchMetaItems(items + guidItem(withGUID))
	if withGUID && isBAD(err) {
		c.caps["X-NO-GUID"] = true
		msgs, err = c.fetchMetaItems(items)
	}

	return msgs, err
}

func (c *imapClient) fetchMetaItems(items string) ([]srcMessage, error) {
	var out []srcMessage

	err := c.cmdEach("UID FETCH 1:* ("+items+")", func(r *response) error {
		kv, ok := fetchItems(r)
		if !ok {
			return nil
		}

		m, ok, err := parseMeta(kv)
		if err != nil {
			return err
		}

		if ok { // not an unsolicited flag update of a concurrent session
			out = append(out, m)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })

	return out, nil
}

// fetchItems returns the item list of a "* n FETCH (...)" response.
func fetchItems(r *response) ([]value, bool) {
	vs, err := newParser(r, 2).values()
	if err != nil || len(vs) < 3 {
		return nil, false
	}

	if kind, _ := str(vs[1]); !strings.EqualFold(kind, "FETCH") {
		return nil, false
	}

	kv, ok := vs[2].([]value)

	return kv, ok
}

// parseMeta reads the identity items of a FETCH response; ok is false
// for a response that does not carry the requested items (UID and
// RFC822.SIZE), such as an unsolicited flag update.
func parseMeta(kv []value) (srcMessage, bool, error) {
	var (
		m       srcMessage
		hasSize bool
	)

	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := str(kv[i])
		k = strings.ToUpper(k)
		hasSize = hasSize || k == itemSize

		if err := m.set(k, kv[i+1]); err != nil {
			return m, false, err
		}
	}

	return m, m.UID != 0 && hasSize, nil
}

// set stores one FETCH item.
func (m *srcMessage) set(k string, v value) error {
	switch k {
	case itemUID:
		n, _ := num(v)
		m.UID = uint32(n)
	case "FLAGS":
		list, _ := v.([]value)
		for _, f := range list {
			if s, ok := str(f); ok && !strings.EqualFold(s, `\Recent`) {
				m.Flags = append(m.Flags, s)
			}
		}
	case "MODSEQ":
		if list, ok := v.([]value); ok && len(list) == 1 {
			m.ModSeq, _ = num(list[0])
		}
	case itemSize:
		n, _ := num(v)
		m.Size = int64(n)
	case itemXGUID:
		m.GUID, _ = str(v)
	case "INTERNALDATE", itemSave:
		return m.setDate(k, v)
	}

	return nil
}

// setDate stores INTERNALDATE or SAVEDATE (NIL is skipped).
func (m *srcMessage) setDate(k string, v value) error {
	s, ok := str(v)
	if !ok {
		return nil
	}

	t, err := time.Parse(imapDate, s)
	if err != nil {
		return fmt.Errorf("uid %d: bad %s %q", m.UID, k, s)
	}

	if k == itemSave {
		m.SaveDate = t
	} else {
		m.InternalDate = t
	}

	return nil
}

// fetchBodies reads the messages of uids from the examined mailbox.
func (c *imapClient) fetchBodies(uids []uint32) (map[uint32][]byte, error) {
	out := make(map[uint32][]byte, len(uids))

	err := c.cmdEach("UID FETCH "+uidSet(uids)+" (UID BODY.PEEK[])", func(r *response) error {
		kv, ok := fetchItems(r)
		if !ok {
			return nil
		}

		var (
			uid  uint32
			body []byte
		)

		for i := 0; i+1 < len(kv); i += 2 {
			k, _ := str(kv[i])

			switch strings.ToUpper(k) {
			case itemUID:
				n, _ := num(kv[i+1])
				uid = uint32(n)
			case "BODY[]":
				switch b := kv[i+1].(type) {
				case []byte:
					body = b
				case qstring:
					body = []byte(b)
				}
			}
		}

		if uid != 0 && body != nil {
			out[uid] = body
		}

		return nil
	})

	return out, err
}

// getACL reads the ACL of a mailbox (identifier to rights).
func (c *imapClient) getACL(raw string) (map[string]string, error) {
	rs, err := c.cmd("GETACL " + quote(raw))
	if err != nil {
		return nil, err
	}

	out := map[string]string{}

	for _, r := range rs {
		vs, err := newParser(r, 2).values()
		if err != nil || len(vs) < 2 {
			continue
		}

		if kind, _ := str(vs[0]); !strings.EqualFold(kind, "ACL") {
			continue
		}

		for i := 2; i+1 < len(vs); i += 2 {
			id, _ := str(vs[i])
			rights, _ := str(vs[i+1])
			out[id] = rights
		}
	}

	return out, nil
}

// uidSet renders ascending UIDs as a compact sequence set.
func uidSet(uids []uint32) string {
	var b strings.Builder

	for i := 0; i < len(uids); {
		j := i
		for j+1 < len(uids) && uids[j+1] == uids[j]+1 {
			j++
		}

		if b.Len() > 0 {
			b.WriteByte(',')
		}

		if j > i {
			fmt.Fprintf(&b, "%d:%d", uids[i], uids[j])
		} else {
			fmt.Fprintf(&b, "%d", uids[i])
		}

		i = j + 1
	}

	return b.String()
}
