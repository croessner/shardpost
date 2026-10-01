package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"shardpost/internal/model"
	"shardpost/internal/sieve"
	"shardpost/internal/version"
)

// SieveScript (RFC 9661). The scripts are the ones ManageSieve manages,
// in the same store, with the same limits and compiled with the same
// hooks. What JMAP adds is an id that does not change when the script is
// renamed; ManageSieve knows scripts by name only. The ids are kept in a
// server-managed metadata entry of the account (name → id), which only
// this front-end writes; a script that appears without one — stored over
// ManageSieve, or by an import — gets one the next time a JMAP client
// lists the scripts.
//
// A script's content is a blob whose id is derived from the content, so a
// new content is a new blobId, and the id is resolvable through the
// download endpoint and Blob/get while a script has that content.
//
// Script writes are not recorded in the account change log: the state is
// a hash of the list, and there is no push type for it.

// CapSieve is the capability of the SieveScript data type (RFC 9661
// 1.2.1).
const CapSieve = "urn:ietf:params:jmap:sieve"

// SieveOptions configure the SieveScript data type; they are
// ManageSieve's, so that both protocols accept the same scripts.
type SieveOptions struct {
	// Config are the Sieve settings ManageSieve compiles with, the
	// capability switches (imapsieve, extprograms) included.
	Config map[string]string
	// GlobalScripts is the directory of "include :global" scripts.
	GlobalScripts string
	// MaxScript bounds a script in bytes; 0 is the store's bound.
	MaxScript int64
}

// typeSieveScript is the name of the data type.
const typeSieveScript = "SieveScript"

// sieveScriptsEntry is the metadata entry the script ids are kept in.
const sieveScriptsEntry = model.ServerManagedPrefix + "sievescripts"

// The property and set errors of RFC 9661 2.
const (
	propIsActive     = "isActive"
	setInvalidSieve  = "invalidSieve"
	setSieveIsActive = "sieveIsActive"
	setAlreadyExists = "alreadyExists"
)

// sieveProperties are the properties of a SieveScript.
var sieveProperties = map[string]bool{propID: true, propName: true, propBlobID: true, propIsActive: true}

// scriptRetries bounds how often a write of the script ids starts over
// when another one changed them under it.
const scriptRetries = 5

// sieveCapability is the value of the sieve capability in an account
// (RFC 9661 1.2.1).
type sieveCapability struct {
	MaxNumberScripts    *int     `json:"maxNumberScripts"`
	SieveExtensions     []string `json:"sieveExtensions"`
	NotificationMethods []string `json:"notificationMethods"`
	ExternalLists       []string `json:"externalLists"`
	MaxSizeScriptName   int      `json:"maxSizeScriptName"`
	MaxSizeScript       int64    `json:"maxSizeScript"`
	MaxNumberRedirects  int      `json:"maxNumberRedirects"`
}

// sieveSessionCapability is the value in the session: the implementation.
func sieveSessionCapability() map[string]string {
	return map[string]string{"implementation": sieve.Implementation + " " + version.Version}
}

// sieveCapability is what the account says about its scripts. ManageSieve
// counts no scripts, so neither does this; extlists does not exist here,
// and enotify knows mailto.
func (s *Server) sieveCapability() *sieveCapability {
	return &sieveCapability{
		SieveExtensions:     sieve.ExtensionList(s.o.Sieve.Config),
		NotificationMethods: []string{"mailto"},
		MaxSizeScriptName:   model.MaxScriptName,
		MaxSizeScript:       s.o.Sieve.MaxScript,
		MaxNumberRedirects:  sieve.MaxRedirects,
	}
}

// sieveOf refuses the methods when the data type is not offered.
func sieveOf(sess *session) (*SieveOptions, *methodError) {
	if sess.srv == nil || sess.srv.o.Sieve == nil {
		return nil, &methodError{Type: errUnknownMethod}
	}

	return sess.srv.o.Sieve, nil
}

// sieveHooks are the compile hooks of the account: includes resolve
// against its own scripts and the global directory, as for ManageSieve.
func sieveHooks(sess *session, o *SieveOptions) sieve.Hooks {
	return sieve.Hooks{Include: sieve.IncludeLoader(sess.user, o.GlobalScripts), Config: o.Config}
}

// scriptBlobPrefix starts the blob id of a script's content; the rest is
// a hash of the content, so that it cannot be mistaken for a GUID or a
// body part.
const scriptBlobPrefix = "S"

// scriptBlobHex is how many hex digits of the hash the blob id carries.
const scriptBlobHex = 40

// scriptBlobID is the blob id of a script's content.
func scriptBlobID(text string) string {
	sum := sha256.Sum256([]byte(text))

	return scriptBlobPrefix + hex.EncodeToString(sum[:])[:scriptBlobHex]
}

// isScriptBlob reports whether a blob id is one scriptBlobID makes.
func isScriptBlob(id string) bool {
	rest, ok := strings.CutPrefix(id, scriptBlobPrefix)
	if !ok || len(rest) != scriptBlobHex {
		return false
	}

	_, err := hex.DecodeString(rest)

	return err == nil && strings.ToLower(rest) == rest
}

// scriptContent is the content of the script whose blob id is given, from
// any script of the account that has it.
func scriptContent(sess *session, id string, limit int64) ([]byte, error) {
	names, _, err := sess.user.ListScripts()
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		text, err := sess.user.GetScript(name)

		switch {
		case errors.Is(err, model.ErrNoScript):
			continue
		case err != nil:
			return nil, err
		case scriptBlobID(text) != id:
			continue
		case limit > 0 && int64(len(text)) > limit:
			return nil, errBlobTooLarge
		}

		return []byte(text), nil
	}

	return nil, model.ErrNoBlob
}

// sieveScript is one script with its id.
type sieveScript struct {
	id     string
	name   string
	text   string
	active bool
}

// render is the object with the properties asked for.
func (sc *sieveScript) render(props *[]string) map[string]any {
	all := map[string]any{propID: sc.id, propName: sc.name, propBlobID: scriptBlobID(sc.text), propIsActive: sc.active}
	out := map[string]any{propID: sc.id}

	for p, v := range all {
		if selected(props, p) {
			out[p] = v
		}
	}

	return out
}

// scriptBook is the account's scripts as JMAP sees them, in name order.
type scriptBook struct {
	list []*sieveScript
}

// byID finds a script by its id.
func (b *scriptBook) byID(id string) *sieveScript {
	for _, sc := range b.list {
		if sc.id == id {
			return sc
		}
	}

	return nil
}

// byName finds a script by its name.
func (b *scriptBook) byName(name string) *sieveScript {
	for _, sc := range b.list {
		if sc.name == name {
			return sc
		}
	}

	return nil
}

// names are the names of the scripts.
func (b *scriptBook) names() map[string]bool {
	out := make(map[string]bool, len(b.list))
	for _, sc := range b.list {
		out[sc.name] = true
	}

	return out
}

// active is the active script, or nil.
func (b *scriptBook) active() *sieveScript {
	for _, sc := range b.list {
		if sc.active {
			return sc
		}
	}

	return nil
}

// state is a hash of every script's id, name, content and activity: it
// changes with anything a SieveScript/get would show differently.
func (b *scriptBook) state() string {
	h := sha256.New()

	for _, sc := range b.list {
		active := "0"
		if sc.active {
			active = "1"
		}

		_, _ = h.Write([]byte(sc.id + "\x00" + sc.name + "\x00" + scriptBlobID(sc.text) + "\x00" + active + "\n"))
	}

	return hex.EncodeToString(h.Sum(nil))[:24]
}

// loadScripts reads the scripts and their ids. It writes nothing: a
// script's id is derived from its name unless the record says otherwise
// (see assignScriptIDs), so a script stored over ManageSieve has an id
// without anything being written for it.
func loadScripts(sess *session) (*scriptBook, error) {
	names, active, err := sess.user.ListScripts()
	if err != nil {
		return nil, err
	}

	_, rec := readScriptIDs(sess)
	b := &scriptBook{}

	var listed []string

	for _, name := range names {
		text, err := sess.user.GetScript(name)
		if errors.Is(err, model.ErrNoScript) {
			continue
		}

		if err != nil {
			return nil, err
		}

		listed = append(listed, name)
		b.list = append(b.list, &sieveScript{name: name, text: text, active: name == active})
	}

	ids := assignScriptIDs(listed, rec)
	for _, sc := range b.list {
		sc.id = ids[sc.name]
	}

	return b, nil
}

// derivedScriptID is the id a script has by its name. k counts the
// attempts after a collision, which only a record that had to shed
// entries can cause (see fitScriptIDs). A derived id starts with a letter
// no random one (hex) has, so the two kinds never collide.
func derivedScriptID(name string, k int) string {
	if k > 0 {
		name += "\x00" + strconv.Itoa(k)
	}

	sum := sha256.Sum256([]byte(name))

	return "n" + hex.EncodeToString(sum[:8])
}

// assignScriptIDs gives every script its id: the one the record keeps
// for its name, else the one derived from the name. The record holds the
// ids that differ from that — scripts created over JMAP, which get a
// random id, scripts renamed over JMAP, which keep theirs, and the names
// a script left over JMAP, which keep a fresh id waiting so that the
// name never hands out the old one again. An id the record keeps for a
// name that is not a script now is reserved: it belongs to a creation or
// a rename that has registered it and not yet written the script.
func assignScriptIDs(names []string, rec map[string]string) map[string]string {
	out := make(map[string]string, len(names))
	used := map[string]bool{}
	listed := map[string]bool{}

	for _, name := range names {
		listed[name] = true

		if id := rec[name]; id != "" && !used[id] {
			out[name], used[id] = id, true
		}
	}

	for name, id := range rec {
		if !listed[name] {
			used[id] = true
		}
	}

	for _, name := range names {
		for k := 0; out[name] == ""; k++ {
			if id := derivedScriptID(name, k); !used[id] {
				out[name], used[id] = id, true
			}
		}
	}

	return out
}

// errScriptIDsBusy is the answer when the ids keep changing under a write.
var errScriptIDsBusy = errors.New("the script ids keep changing; try again")

// readScriptIDs reads the stored ids, and the record they came from. A
// record that does not read is treated as none.
func readScriptIDs(sess *session) (string, map[string]string) {
	ids := map[string]string{}

	md, err := sess.user.GetMetadata("", sess.account)
	if err != nil {
		sess.log.Warn("jmap: the script ids could not be read", "err", err)

		return "", ids
	}

	raw := md[sieveScriptsEntry]
	if raw != "" && json.Unmarshal([]byte(raw), &ids) != nil {
		sess.log.Warn("jmap: the stored script ids do not read")

		ids = map[string]string{}
	}

	return raw, ids
}

// maxScriptIDRecord bounds the record of the ids, like the identities'.
var maxScriptIDRecord = maxIdentityRecord

// writeScriptIDs replaces the stored ids while they are still raw, and
// returns the ids that were kept. An entry that says what the name says
// anyway is not stored; see fitScriptIDs for a record beyond its bound.
// listed are the names of the scripts.
func writeScriptIDs(sess *session, raw string, ids map[string]string, listed map[string]bool) (map[string]string, bool, error) {
	for name, id := range ids {
		if id == derivedScriptID(name, 0) {
			delete(ids, name)
		}
	}

	ids = fitScriptIDs(sess, ids, listed)

	var next *string

	if len(ids) > 0 {
		b, err := json.Marshal(ids)
		if err != nil {
			return nil, false, err
		}

		rec := string(b)
		next = &rec
	}

	if (next == nil && raw == "") || (next != nil && *next == raw) {
		return ids, true, nil
	}

	swapped, err := sess.user.SwapMetadata("", sess.account, sieveScriptsEntry, raw, next)

	return ids, swapped, err
}

// scriptIDsOverflowed is set once the record had to shed entries, so that
// the warning is logged once per process rather than with every write.
var scriptIDsOverflowed atomic.Bool

// fitScriptIDs keeps the record within its bound, as a last resort: the
// record holds only the ids that differ from the derived ones, so it
// takes an account with hundreds of scripts renamed or created over JMAP
// to get here. Entries of names that are not scripts go first — the
// waiting ids of names scripts left, which then may hand out an old id
// again; if that is not enough, the entries of the last scripts by name,
// whose ids then fall back to the derived ones.
func fitScriptIDs(sess *session, ids map[string]string, listed map[string]bool) map[string]string {
	if scriptIDRecordSize(ids) <= maxScriptIDRecord {
		return ids
	}

	names := make([]string, 0, len(ids))
	for name := range ids {
		names = append(names, name)
	}

	// Unlisted names first, then the listed ones from the end.
	sort.Slice(names, func(i, j int) bool {
		if li, lj := listed[names[i]], listed[names[j]]; li != lj {
			return lj
		}

		return names[i] > names[j]
	})

	for _, name := range names {
		if scriptIDRecordSize(ids) <= maxScriptIDRecord {
			break
		}

		delete(ids, name)
	}

	if !scriptIDsOverflowed.Swap(true) {
		sess.log.Warn("jmap: the Sieve script ids outgrew their record; some fall back to ids derived from names")
	}

	return ids
}

// scriptIDRecordSize is the size of the record the ids make.
func scriptIDRecordSize(ids map[string]string) int {
	b, _ := json.Marshal(ids)

	return len(b)
}

// changeScriptIDs applies a change to the stored ids, starting over when
// another write came first, and returns the ids that were kept. listed
// are the names of the scripts, as for writeScriptIDs.
func changeScriptIDs(sess *session, listed map[string]bool, change func(ids map[string]string)) (map[string]string, error) {
	for range retriesFor(sess, scriptRetries) {
		raw, ids := readScriptIDs(sess)
		change(ids)

		kept, swapped, err := writeScriptIDs(sess, raw, ids, listed)
		if err != nil || swapped {
			return kept, err
		}
	}

	return nil, errScriptIDsBusy
}

// sieveScriptGet implements SieveScript/get (RFC 9661 2.3). Without ids
// it lists every script, which is what ManageSieve's LISTSCRIPTS is.
func sieveScriptGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	if _, merr := sieveOf(sess); merr != nil {
		return nil, merr
	}

	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	if merr := checkRequested(req.Properties, sieveProperties); merr != nil {
		return nil, merr
	}

	b, err := loadScripts(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	out := &getResponse{AccountID: accountID(sess.account), State: b.state(), List: []map[string]any{}, NotFound: []string{}}

	if req.IDs == nil {
		for _, sc := range b.list {
			out.List = append(out.List, sc.render(req.Properties))
		}

		return out, nil
	}

	for _, id := range *req.IDs {
		if sc := b.byID(id); sc != nil {
			out.List = append(out.List, sc.render(req.Properties))
		} else {
			out.NotFound = append(out.NotFound, id)
		}
	}

	return out, nil
}

// sieveFilter is the filter of a SieveScript/query (RFC 9661 2.5).
type sieveFilter struct {
	Name       *string       `json:"name"`
	IsActive   *bool         `json:"isActive"`
	Operator   string        `json:"operator"`
	Conditions []sieveFilter `json:"conditions"`
}

// operator implements opFilter.
func (f *sieveFilter) operator() string { return f.Operator }

// conditionCount implements opFilter.
func (f *sieveFilter) conditionCount() int { return len(f.Conditions) }

// sieveChild is the i-th filter below an operator.
func sieveChild(f *sieveFilter, i int) *sieveFilter { return &f.Conditions[i] }

// condition evaluates a condition: the name contains the text (without
// regard to case, as i;ascii-casemap does), and the activity is the one
// given.
func (f *sieveFilter) condition(sc *sieveScript) bool {
	switch {
	case f.Name != nil && !strings.Contains(collate(collationCasemap, sc.name), collate(collationCasemap, *f.Name)):
		return false
	case f.IsActive != nil && *f.IsActive != sc.active:
		return false
	}

	return true
}

// sieveFilterKeys are the properties a SieveScript filter may have.
var sieveFilterKeys = map[string]bool{propName: true, propIsActive: true, filterOperator: true, filterConditions: true}

// sieveSortKeys are the properties RFC 9661 2.5 sorts by.
var sieveSortKeys = map[string]bool{propName: true, propIsActive: true}

// sieveScriptQuery implements SieveScript/query (RFC 9661 2.5). An
// account has a handful of scripts; they are filtered and sorted in
// memory, and ties fall back to the name order they are listed in.
func sieveScriptQuery(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	if _, merr := sieveOf(sess); merr != nil {
		return nil, merr
	}

	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	var filter *sieveFilter

	if len(req.Filter) > 0 && string(req.Filter) != jsonNull {
		filter = &sieveFilter{}
		if merr := decodeOpFilter(req.Filter, filter, sieveFilterKeys, "a Sieve script", sieveChild); merr != nil {
			return nil, merr
		}
	}

	if merr := checkSort(req.Sort, sieveSortKeys); merr != nil {
		return nil, merr
	}

	b, err := loadScripts(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	var matched []*sieveScript

	for _, sc := range b.list {
		if filter == nil || matchOpFilter(filter, func(c *sieveFilter) bool { return c.condition(sc) }, sieveChild) {
			matched = append(matched, sc)
		}
	}

	sortScripts(matched, req.Sort)

	ids := make([]string, 0, len(matched))
	for _, sc := range matched {
		ids = append(ids, sc.id)
	}

	return page(accountID(sess.account), b.state(), ids, &req.paging)
}

// sortScripts applies the comparators, last key first. false sorts
// before true, as it does in every JMAP sort of a Boolean.
func sortScripts(list []*sieveScript, by []comparator) {
	for i := len(by) - 1; i >= 0; i-- {
		c := by[i]

		less := func(a, b *sieveScript) bool {
			if c.Property == propIsActive {
				return !a.active && b.active
			}

			return collate(c.Collation, a.name) < collate(c.Collation, b.name)
		}

		sort.SliceStable(list, func(a, b int) bool {
			if !c.ascending() {
				return less(list[b], list[a])
			}

			return less(list[a], list[b])
		})
	}
}

// sieveScriptQueryChanges answers SieveScript/queryChanges, which RFC
// 9661 does not define and a client may still try: the query is cheap to
// run again, and SieveScript/query says so with canCalculateChanges false.
func sieveScriptQueryChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	if _, merr := sieveOf(sess); merr != nil {
		return nil, merr
	}

	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	return nil, &methodError{Type: errCannotCalculateChanges,
		Description: "this server does not compute script query deltas; run the query again"}
}

// sieveScriptValidate implements SieveScript/validate (RFC 9661 2.6),
// ManageSieve's CHECKSCRIPT: the blob is compiled as SieveScript/set would
// compile it, and nothing is stored.
func sieveScriptValidate(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	o, merr := sieveOf(sess)
	if merr != nil {
		return nil, merr
	}

	var req struct {
		AccountID string `json:"accountId"`
		BlobID    string `json:"blobId"`
	}

	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.BlobID == "" {
		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{propBlobID}, Description: "SieveScript/validate needs a blobId"}
	}

	_, serr := scriptText(sess, o, req.BlobID)

	return map[string]any{propAccountID: accountID(sess.account), "error": serr}, nil
}

// scriptText reads the content of a script from a blob and checks it as
// ManageSieve's PUTSCRIPT does: within the size limit, UTF-8, not empty
// (RFC 9661 2.2), and compiling with the account's hooks. A compile
// error is invalidSieve with the compiler's message, which names the
// line.
func scriptText(sess *session, o *SieveOptions, blobID string) (string, *setError) {
	raw, err := blobContent(sess, blobID, o.MaxScript)

	switch {
	case errors.Is(err, model.ErrNoBlob):
		return "", &setError{Type: setBlobNotFound, NotFound: []string{blobID}}
	case errors.Is(err, errBlobTooLarge) || int64(len(raw)) > o.MaxScript:
		return "", &setError{Type: setTooLarge, Description: "the script is larger than maxSizeScript"}
	case err != nil:
		return "", &setError{Type: setServerFail, Description: err.Error()}
	case len(raw) == 0 || !utf8.Valid(raw):
		return "", &setError{Type: setInvalidSieve, Description: "a script is UTF-8 text of at least one character"}
	}

	text := string(raw)
	if err := sieve.CompileWith(text, sieveHooks(sess, o)); err != nil {
		return "", &setError{Type: setInvalidSieve, Description: err.Error()}
	}

	return text, nil
}
