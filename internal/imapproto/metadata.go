package imapproto

import (
	"fmt"
	"sort"
	"strings"

	"shardpost/internal/model"
)

// METADATA (RFC 5464): GETMETADATA / SETMETADATA for server and mailbox
// annotations. Shared entries need r to read and w to write on shared
// mailboxes; private entries belong to the account and need r.

const maxAnnotationSize = 64 << 10

// serverAccount is the reserved namespace holding shared server-level
// annotations (never a login name: the character is not allowed in
// account names).
const serverAccount = "~server"

// validEntry checks an entry name; the bare scope prefixes "/shared" and
// "/private" are only valid as GETMETADATA (DEPTH) roots, never as an
// entry to set or store.
func validEntry(e string, forSet bool) bool {
	if e == "/shared" || e == "/private" {
		return !forSet
	}

	if !strings.HasPrefix(e, "/shared/") && !strings.HasPrefix(e, "/private/") {
		return false
	}

	if strings.Contains(e, "//") || strings.HasSuffix(e, "/") || strings.ContainsAny(e, "*%") {
		return false
	}

	for i := 0; i < len(e); i++ {
		if e[i] < 0x20 || e[i] > 0x7e {
			return false
		}
	}

	return true
}

// GETMETADATA [(MAXSIZE n DEPTH 0|1|infinity)] mailbox entry|(entries)
func (c *conn) cmdGetMetadata(tag string) error {
	maxSize, depth, t, err := c.readMetadataOptions()
	if err != nil {
		return err
	}

	if !t.isString() {
		return fmt.Errorf("%w: expected mailbox", errSyntax)
	}

	mailbox := t.str

	wanted, err := c.readOneOrList("entries")
	if err != nil {
		return err
	}

	if !c.validEntries(tag, wanted) {
		return nil
	}

	tgt, err := c.metadataTarget(tag, mailbox, 'r')
	if err != nil || tgt == nil {
		return err
	}

	all, err := tgt.u.GetMetadata(tgt.name, c.name)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	if mailbox == "" {
		c.addServerMetadata(all)
	}

	parts, longest := metadataParts(all, matchMetadataKeys(all, wanted, depth), maxSize)
	c.metadataResponse(tag, mailbox, parts, longest)

	return nil
}

// readMetadataOptions reads the optional GETMETADATA option list and
// returns MAXSIZE (-1 when absent), DEPTH and the token after the list.
func (c *conn) readMetadataOptions() (maxSize int64, depth int, t token, err error) {
	maxSize = -1

	t, err = c.r.next()
	if err != nil {
		return maxSize, depth, t, err
	}

	if t.kind == tokList {
		if maxSize, depth, err = parseMetadataOptions(t.list); err != nil {
			return maxSize, depth, t, err
		}

		if t, err = c.r.next(); err != nil {
			return maxSize, depth, t, err
		}
	}

	return maxSize, depth, t, nil
}

// validEntries answers BAD and returns false when an entry name is invalid.
func (c *conn) validEntries(tag string, wanted []string) bool {
	for _, e := range wanted {
		if !validEntry(e, false) {
			c.bad(tag, "Invalid entry name")
			return false
		}
	}

	return true
}

// metadataResponse writes the METADATA response (if any entry is left)
// and the tagged OK, with LONGENTRIES when entries exceeded MAXSIZE.
func (c *conn) metadataResponse(tag, mailbox string, parts []string, longest int64) {
	if len(parts) > 0 {
		c.w.untagged("METADATA %s (%s)", astringOut(mailbox), strings.Join(parts, " "))
	}

	if longest > 0 {
		c.ok(tag, fmt.Sprintf("METADATA LONGENTRIES %d", longest), "GETMETADATA completed")
	} else {
		c.ok(tag, "", "GETMETADATA completed")
	}
}

// parseMetadataOptions parses the GETMETADATA options MAXSIZE (-1 when
// absent) and DEPTH (0, 1, or -1 for infinity).
func parseMetadataOptions(list []token) (maxSize int64, depth int, err error) {
	maxSize = -1

	for i := 0; i+1 < len(list); i += 2 {
		switch strings.ToUpper(list[i].str) {
		case "MAXSIZE":
			var n int64
			if _, err := fmt.Sscanf(list[i+1].str, "%d", &n); err != nil {
				return 0, 0, fmt.Errorf("%w: bad MAXSIZE", errSyntax)
			}

			maxSize = n
		case "DEPTH":
			if depth, err = parseMetadataDepth(list[i+1].str); err != nil {
				return 0, 0, err
			}
		default:
			return 0, 0, fmt.Errorf("%w: unknown GETMETADATA option", errSyntax)
		}
	}

	return maxSize, depth, nil
}

// parseMetadataDepth parses a DEPTH value: 0, 1 or infinity (-1).
func parseMetadataDepth(s string) (int, error) {
	switch strings.ToLower(s) {
	case "0":
		return 0, nil
	case "1":
		return 1, nil
	case "infinity":
		return -1, nil
	}

	return 0, fmt.Errorf("%w: bad DEPTH", errSyntax)
}

// addServerMetadata adds the shared server entries to all. They are
// global: they live in the reserved namespace serverAccount and are
// read-only for accounts.
func (c *conn) addServerMetadata(all map[string]string) {
	su, err := c.other(serverAccount)
	if err != nil {
		return
	}

	shared, err := su.GetMetadata("", "")
	if err != nil {
		return
	}

	for k, v := range shared {
		if strings.HasPrefix(k, "/shared/") {
			all[k] = v
		}
	}
}

// matchMetadataKeys returns the sorted keys of all that one of the wanted
// entries selects at the given depth.
func matchMetadataKeys(all map[string]string, wanted []string, depth int) []string {
	var keys []string

	for k := range all {
		for _, w := range wanted {
			if metadataSelects(k, w, depth) {
				keys = append(keys, k)
				break
			}
		}
	}

	sort.Strings(keys)

	return keys
}

// metadataSelects reports whether entry w selects key k: k itself or,
// with depth, its children (depth 1) or all descendants (depth -1).
func metadataSelects(k, w string, depth int) bool {
	return k == w || (depth != 0 && strings.HasPrefix(k, w+"/") && (depth < 0 || !strings.Contains(k[len(w)+1:], "/")))
}

// metadataParts renders the entries of keys that fit maxSize (-1: no
// limit) and returns the length of the longest one left out.
func metadataParts(all map[string]string, keys []string, maxSize int64) ([]string, int64) {
	var parts []string

	longest := int64(0)

	for _, k := range keys {
		v := all[k]
		if maxSize >= 0 && int64(len(v)) > maxSize {
			if int64(len(v)) > longest {
				longest = int64(len(v))
			}

			continue
		}

		parts = append(parts, astringOut(k), nstringOut(v))
	}

	return parts, longest
}

// SETMETADATA mailbox (entry value [entry value ...])
func (c *conn) cmdSetMetadata(tag string) error {
	mailbox, err := c.r.astring()
	if err != nil {
		return err
	}

	list, err := c.readMetadataList()
	if err != nil {
		return err
	}

	entries, needW, ok, err := c.parseMetadataEntries(tag, list)
	if err != nil || !ok {
		return err
	}

	// The JMAP identities and vacation response are the server's: it
	// checks them and delivery answers from the vacation. Clients read
	// them, and change them over JMAP.
	for e := range entries {
		if model.ServerManagedEntry(e) {
			c.no(tag, "NOPERM", "Entries under "+model.ServerManagedPrefix+" are kept by the server")
			return nil
		}
	}

	right := metadataRight(entries, needW)

	if mailbox == "" && needW {
		c.no(tag, "NOPERM", "Shared server annotations are read-only")
		return nil
	}

	tgt, err := c.metadataTarget(tag, mailbox, right)
	if err != nil || tgt == nil {
		return err
	}

	if !c.storeMetadata(tag, tgt, entries) {
		return nil
	}

	c.ok(tag, "", "SETMETADATA completed")

	return nil
}

// readMetadataList reads the non-empty (entry value ...) list of
// SETMETADATA.
func (c *conn) readMetadataList() ([]token, error) {
	t, err := c.r.next()
	if err != nil {
		return nil, err
	}

	if t.kind != tokList || len(t.list)%2 != 0 || len(t.list) == 0 {
		return nil, fmt.Errorf("%w: expected (entry value ...)", errSyntax)
	}

	return t.list, nil
}

// metadataRight is the right SETMETADATA needs: r for private entries,
// w for shared ones, a for the IMAPSIEVE script.
func metadataRight(entries map[string]*string, needW bool) byte {
	right := byte('r')
	if needW {
		right = 'w'
	}

	if _, ok := entries["/shared/imapsieve/script"]; ok {
		right = 'a'
	}

	return right
}

// storeMetadata writes the entries and answers a failure; false then.
func (c *conn) storeMetadata(tag string, tgt *target, entries map[string]*string) bool {
	if err := tgt.u.SetMetadata(tgt.name, c.name, entries); err != nil {
		if err == model.ErrTooLarge {
			c.no(tag, fmt.Sprintf("METADATA MAXSIZE %d", maxAnnotationSize), "Value too large")
			return false
		}

		c.storeErr(tag, err)

		return false
	}

	return true
}

// parseMetadataEntries parses the SETMETADATA (entry value ...) list; a
// nil value removes the entry. needW is true when a shared entry is
// set; ok is false when the client was answered.
func (c *conn) parseMetadataEntries(tag string, list []token) (entries map[string]*string, needW, ok bool, err error) {
	entries = map[string]*string{}

	for i := 0; i < len(list); i += 2 {
		e, v := list[i], list[i+1]
		if !validEntry(e.str, true) {
			c.bad(tag, "Invalid entry name")
			return nil, false, false, nil
		}

		if strings.HasPrefix(e.str, "/shared/") {
			needW = true
		}

		if v.kind == tokAtom && strings.EqualFold(v.str, kwNIL) {
			entries[e.str] = nil
			continue
		}

		if !v.isString() {
			return nil, false, false, fmt.Errorf("%w: expected value", errSyntax)
		}

		if len(v.str) > maxAnnotationSize {
			c.no(tag, fmt.Sprintf("METADATA MAXSIZE %d", maxAnnotationSize), "Value too large")
			return nil, false, false, nil
		}

		val := v.str
		entries[e.str] = &val
	}

	return entries, needW, true, nil
}

// metadataTarget resolves the mailbox ("" = server) and checks the right.
// It answers the client itself and returns nil when the command must stop.
func (c *conn) metadataTarget(tag, mailbox string, right byte) (*target, error) {
	if mailbox == "" {
		return &target{u: c.user}, nil
	}

	t, err := c.target(mailbox)
	if err != nil {
		c.storeErr(tag, err)
		return nil, nil
	}

	if _, err := t.u.Lookup(t.name); err != nil {
		c.storeErr(tag, err)
		return nil, nil
	}

	if !c.needRight(tag, t, right) {
		return nil, nil
	}

	return t, nil
}

// nstringOut renders a value as quoted string or literal.
func nstringOut(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] == '\r' || v[i] == '\n' || v[i] == 0 || v[i] >= 0x80 {
			return literalString([]byte(v))
		}
	}

	return quote(v)
}
