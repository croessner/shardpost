package interp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/textproto"
	"strings"

	"github.com/foxcpp/go-sieve/lexer"
)

type PolicyReader interface {
	RedirectAllowed(ctx context.Context, d *RuntimeData, addr string) (bool, error)
}

// Env provides access to named environment items for the
// environment test (RFC 5183). Implementations return ("", false) for unknown
// or unavailable items.
type Env interface {
	// GetEnvironment returns the value of the named item and whether it exists.
	GetEnvironment(name string) (value string, ok bool)
}

type Envelope interface {
	EnvelopeFrom() string
	EnvelopeTo() string
	AuthUsername() string
}

type Message interface {
	/*
		HeaderGet returns the header field value.

		RFC requires the following handling for encoded fields:

		      Comparisons are performed on octets.  Implementations convert text
		      from header fields in all charsets [MIME3] to Unicode, encoded as
		      UTF-8, as input to the comparator (see section 2.7.3).
		      Implementations MUST be capable of converting US-ASCII, ISO-8859-
		      1, the US-ASCII subset of ISO-8859-* character sets, and UTF-8.
		      Text that the implementation cannot convert to Unicode for any
		      reason MAY be treated as plain US-ASCII (including any [MIME3]
		      syntax) or processed according to local conventions.  An encoded
		      NUL octet (character zero) SHOULD NOT cause early termination of
		      the header content being compared against.
	*/
	HeaderGet(key string) ([]string, error)
	MessageSize() int
}

type BodyPart interface {
	ContentType() string
	Open(ctx context.Context) (io.ReadCloser, error)
}

// BodyPartBytes is the minimal implementation of BodyPart
// that uses []byte to store whole body part.
type BodyPartBytes struct {
	Blob             []byte
	ContentTypeValue string
}

func (b BodyPartBytes) ContentType() string {
	return b.ContentTypeValue
}

func (b BodyPartBytes) Open(ctx context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.Blob)), nil
}

type BodyPartRaw struct {
	BodyMessage
}

func (BodyPartRaw) ContentType() string {
	return ""
}

func (b BodyPartRaw) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := b.BodyRaw(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNoBody
	}
	return io.NopCloser(r), nil
}

// BodyMessage is an optional extension of the Message interface that provides
// access to the message body for the body extension (RFC 5173).
//
// Implementations should implement this interface to support body tests.
// If the Message does not implement BodyMessage, body tests will always
// return false (no body available).
type BodyMessage interface {
	// BodyRaw returns the raw, undecoded body of the message as a stream.
	// This is used for the :raw body transform.
	// The body starts after the first empty line following the headers.
	// The returned reader must be closed by the caller.
	// If there is no body (header-only message without empty line separator),
	// it should return nil, nil.
	BodyRaw(ctx context.Context) (io.Reader, error)

	// BodyParts returns decoded MIME parts whose content-type matches any of
	// the given patterns. This is used for the :content and :text body transforms.
	//
	// Each pattern may be:
	//   - "" (empty string): matches all content types
	//   - "type" (no slash): matches any subtype of the given type
	//   - "type/subtype": matches exactly that content-type pair
	//   - patterns beginning/ending with '/' or with multiple slashes: match nothing
	// ContentTypeMatches function can be used to help determine whether each
	// content-type of each body part matches provided patterns.
	//
	// For multipart/* parts: returns the prologue and epilogue as separate strings.
	// For message/rfc822 parts: returns only the header as a single string.
	// For other parts: returns the decoded body content as UTF-8.
	//
	// Returns nil slice (not an error) if no matching parts are found.
	//
	// If Content-Transfer-Encoding is used for a part, the returned reader is expected to
	// be decoded accordingly (e.g. base64 or quoted-printable decoded).
	//
	// Since Sieve operates on UTF-8 or 7-bit ASCII strings, the returned readers
	// should provide decoded content as UTF-8, performing necessary decoding
	// if a non-Unicode charset is specified in Content-Type.
	BodyParts(ctx context.Context, contentTypes []string) ([]BodyPart, error)
}

type ExecuteTestMessage struct {
	Envelope Envelope
	Message  Message
	Flags    []string
}

// ExecuteTestEnvironment is a mock MTA interface used for integration tests. It is possible to test
// the actual execution of actions (fileinto, redirect, keep, etc.) using this interface in a particular
// MTA implementation.
type ExecuteTestEnvironment interface {
	CreateMailbox(name string) error
	// GetDefaultMailbox returns the mailbox name used by keep action.
	GetDefaultMailbox() string
	ExecuteActions(d *RuntimeData, actions []AppliedAction) error
	// GetSMTPMessage returns the message that was enqueued for delivery using redirect or other
	// similar command.
	GetSMTPMessage(index int) (*ExecuteTestMessage, error)
	HasSMTPMessage(index int) (bool, error)
	// GetMailboxMessage returns the message that was saved using either fileinfo or keep command.
	GetMailboxMessage(mailboxName string, index int) (*ExecuteTestMessage, error)
	HasMailboxMessage(mailboxName string, index int) (bool, error)
}

type TestRuntime struct {
	ProgramFactory func(*RuntimeData) ProgramRunner
	Name           string
	FailMessage    string
	FailAt         lexer.Position
	Script         *Script
	SavedScripts   map[string][]byte
	MaxNesting     int
	Execute        ExecuteTestEnvironment
	// Metadata set by test_imap_metadata_set: "mailbox\x00annotation".
	Metadata map[string]string
	// LastErrors is 1 when the last test_script_compile/test_script_run
	// failed (test_error :count).
	LastErrors  int
	RunOriginal Message
	Executed    int

	// Msg, Envelope from RuntimeData saved here when test_message
	// is used.
	OriginalEnvelope Envelope
	OriginalMsg      Message
	OriginalFlags    Flags
}

type RuntimeData struct {
	runContext context.Context
	mimeErr    error

	Policy   PolicyReader
	Envelope Envelope
	Msg      Message
	Script   *Script
	Env      Env
	// For files accessible vis "include", "test_script_compile", etc.
	Namespace fs.FS

	// Hooks for extensions (shardpost fork); nil means unsupported at
	// runtime (the test then evaluates to false / the command is a no-op).
	// MailboxExists backs the "mailboxexists" test (RFC 5490).
	MailboxExists func(ctx context.Context, name string) (bool, error)
	// Duplicate backs the "duplicate" test (RFC 7352): it reports whether
	// key was seen before and records it for seconds.
	Duplicate func(ctx context.Context, key string, seconds int) (bool, error)
	// HeaderEdits records addheader/deleteheader (RFC 5293) in script
	// order; the caller applies them to the stored message. Tests within
	// the script see edited headers through Msg when it implements
	// HeaderEditor.
	HeaderEdits []HeaderEdit
	// Metadata backs the mboxmetadata/servermetadata tests (RFC 5490):
	// mailbox "" asks for a server annotation. ok is false if the
	// annotation does not exist.
	Metadata func(ctx context.Context, mailbox, annotation string) (value string, ok bool, err error)
	// SpecialUseExists backs the specialuse_exists test (RFC 8579):
	// mailbox "" asks whether any mailbox carries each of the uses.
	SpecialUseExists func(ctx context.Context, mailbox string, uses []string) (bool, error)

	// ActionMsgs holds, per applied action, the message as it was when
	// the action ran (header edits in script order matter: a redirect
	// before a deleteheader forwards the header). nil entries mean "the
	// final message".
	ActionMsgs []Message

	// MIME tree of the current message (mime extension) and the parts a
	// foreverypart loop is at.
	mimeCache    *MimePart
	mimeCacheLen int
	mimeCachePtr *byte
	partStack    []*MimePart

	// Program runs external programs (vnd.dovecot.execute/pipe/filter);
	// nil: the extensions fail at runtime. Replaced is the message a
	// filter program produced (nil: unchanged).
	Program  ProgramRunner
	Replaced []byte

	// RuntimeErrors counts errors that were reported but did not stop the
	// script (an invalid regex from a variable).
	RuntimeErrors int

	// Globals are the variables shared between a script and the scripts
	// it includes (RFC 6609 "global"); IncludedOnce records the scripts
	// already included (":once").
	Globals      map[string]string
	IncludedOnce map[string]bool

	ifResult bool

	// OnAction is called each time a Sieve script requests some action. If an error is
	// returned - script execution is aborted. Default handler (set by NewRuntimeData)
	// appends applied action to AppliedActions. If OnAction is replaced, it must
	// still append all actions to AppliedActions as it is used for some internal checks.
	OnAction       func(ctx context.Context, action AppliedAction, d *RuntimeData) error
	AppliedActions []AppliedAction

	RedirectAddr []string // Deprecated: Use AppliedActions instead.
	Mailboxes    []string // Deprecated: Use AppliedActions instead.
	Flags        []string // Default list of flags applied for actions.
	Keep         bool     // Deprecated: Use AppliedActions instead.
	ImplicitKeep bool     // Deprecated: Use AppliedActions instead.

	FlagAliases map[string]string

	MatchVariables []string
	Variables      map[string]string

	// vnd.dovecot.testsuite state, not intended for production use
	Test *TestRuntime
}

func (d *RuntimeData) Copy() *RuntimeData {
	newData := &RuntimeData{
		Policy:           d.Policy,
		Envelope:         d.Envelope,
		Msg:              snapshotMessage(d.Msg),
		Script:           d.Script,
		Env:              d.Env,
		Namespace:        d.Namespace,
		MailboxExists:    d.MailboxExists,
		Duplicate:        d.Duplicate,
		Metadata:         d.Metadata,
		Program:          d.Program,
		SpecialUseExists: d.SpecialUseExists,
		Globals:          d.Globals,
		IncludedOnce:     d.IncludedOnce,
		HeaderEdits:      d.HeaderEdits,
		OnAction:         d.OnAction,
		AppliedActions:   make([]AppliedAction, len(d.AppliedActions)),
		ActionMsgs:       append([]Message{}, d.ActionMsgs...),
		partStack:        append([]*MimePart{}, d.partStack...),
		RedirectAddr:     make([]string, len(d.RedirectAddr)),
		Mailboxes:        make([]string, len(d.Mailboxes)),
		Keep:             d.Keep,
		ImplicitKeep:     d.ImplicitKeep,
		FlagAliases:      make(map[string]string, len(d.FlagAliases)),
		MatchVariables:   make([]string, len(d.MatchVariables)),
		Variables:        make(map[string]string, len(d.Variables)),
		Test:             d.Test,
	}

	copy(newData.AppliedActions, d.AppliedActions)
	copy(newData.RedirectAddr, d.RedirectAddr)
	copy(newData.Mailboxes, d.Mailboxes)
	if d.Flags != nil {
		newData.Flags = append([]string(nil), d.Flags...)
	}
	copy(newData.MatchVariables, d.MatchVariables)

	for k, v := range d.FlagAliases {
		newData.FlagAliases[k] = v
	}
	for k, v := range d.Variables {
		newData.Variables[k] = v
	}

	if newData.Test != nil {
		c := *newData.Test
		newData.Test = &c
	}
	if newData.Test != nil && newData.Test.ProgramFactory != nil {
		newData.Program = newData.Test.ProgramFactory(newData)
	}
	return newData
}

func (d *RuntimeData) MatchVariable(i int) string {
	if i >= len(d.MatchVariables) {
		return ""
	}
	return d.MatchVariables[i]
}

func (d *RuntimeData) Var(name string) (string, error) {
	namespace, name, ok := strings.Cut(strings.ToLower(name), ".")
	if !ok {
		name = namespace
		namespace = ""
	}

	switch namespace {
	case "envelope":
		// >  References to namespaces without a prior require statement for the
		// >  relevant extension MUST cause an error.
		if !d.Script.RequiresExtension("envelope") {
			return "", fmt.Errorf("require 'envelope' to use corresponding variables")
		}
		switch name {
		case "from":
			return d.Envelope.EnvelopeFrom(), nil
		case "to":
			return d.Envelope.EnvelopeTo(), nil
		case "auth":
			return d.Envelope.AuthUsername(), nil
		default:
			return "", nil
		}
	case "global":
		if !d.Script.RequiresExtension("include") {
			return "", fmt.Errorf("require 'include' to use global variables")
		}
		return d.Globals[name], nil
	case "tst": // testsuite: tst.path is the directory of the test file
		if name == "path" {
			return d.Script.opts.TestPath, nil
		}
		return "", nil
	case "":
		// User variables; a name declared global lives in the shared set.
		if d.Script.IsGlobal(name) {
			return d.Globals[name], nil
		}
		return d.Variables[name], nil
	default:
		return "", fmt.Errorf("unknown extension variable: %v", name)
	}
}

func (d *RuntimeData) SetVar(name, value string) error {
	if len(name) > d.Script.opts.MaxVariableNameLen {
		return fmt.Errorf("attempting to use a too long variable name: %v", name)
	}
	if len(value) > d.Script.opts.MaxVariableLen {
		until := d.Script.opts.MaxVariableLen
		// If this truncated an otherwise valid Unicode character,
		// remove the character altogether.
		for until > 0 && value[until] >= 128 && value[until] < 192 /* second or further octet of UTF-8 encoding */ {
			until--
		}

		value = value[:until]

	}

	namespace, name, ok := strings.Cut(strings.ToLower(name), ".")
	if !ok {
		name = namespace
		namespace = ""
	}

	switch namespace {
	case "envelope":
		return fmt.Errorf("cannot modify envelope. variables")
	case "global":
		if !d.Script.RequiresExtension("include") {
			return fmt.Errorf("require 'include' to use global variables")
		}
		d.setGlobal(name, value)
		return nil
	case "":
		// User variables.
		if d.Script.IsGlobal(name) {
			d.setGlobal(name, value)
			return nil
		}
		d.Variables[name] = value
		return nil
	default:
		return fmt.Errorf("unknown extension variable: %v", name)
	}
}

func (d *RuntimeData) setGlobal(name, value string) {
	if d.Globals == nil {
		d.Globals = map[string]string{}
	}
	d.Globals[name] = value
}

// DefaultMaxActions bounds the actions of one script (Pigeonhole's
// sieve_max_actions default).
const DefaultMaxActions = 32

func DefaultOnAction(ctx context.Context, action AppliedAction, d *RuntimeData) error {
	if err := d.checkExecution(); err != nil {
		return err
	}
	if err := checkActionConflict(d, action); err != nil {
		return err
	}
	d.AppliedActions = append(d.AppliedActions, action)
	d.ActionMsgs = append(d.ActionMsgs, snapshotMessage(d.Msg))
	return nil
}

// snapshotMessage copies an in-memory message so that later header edits
// do not change what an earlier action delivers; other message types are
// used as they are.
func snapshotMessage(m Message) Message {
	if ms, ok := m.(*MessageStatic); ok {
		c := *ms
		if h, ok := ms.Header.(textproto.MIMEHeader); ok {
			hc := textproto.MIMEHeader{}
			for k, v := range h {
				hc[k] = append([]string{}, v...)
			}
			c.Header = hc
		}
		return &c
	}
	return m
}

// ActionMessage returns the message an action delivers (see ActionMsgs).
func (d *RuntimeData) ActionMessage(i int) Message {
	if i >= 0 && i < len(d.ActionMsgs) && d.ActionMsgs[i] != nil {
		return d.ActionMsgs[i]
	}
	return d.Msg
}

// checkActionConflict enforces RFC 5429 2.1 (reject/ereject exclude the
// delivering actions), RFC 5230 4.1 (one vacation per script) and the
// action limit.
func checkActionConflict(d *RuntimeData, action AppliedAction) error {
	max := DefaultMaxActions
	if d.Script != nil {
		if v := d.Script.opts.Conf("sieve_max_actions"); v != "" {
			fmt.Sscanf(v, "%d", &max)
		}
		if d.Script.opts.MaxActions > 0 {
			max = d.Script.opts.MaxActions
		}
	}
	if _, discard := action.(ActionDiscard); !discard && len(d.AppliedActions) >= max {
		return fmt.Errorf("total number of actions exceeds policy limit (%d)", max)
	}
	isReject := func(a AppliedAction) bool {
		switch a.(type) {
		case ActionReject, ActionEReject:
			return true
		}
		return false
	}
	delivers := func(a AppliedAction) bool {
		switch x := a.(type) {
		case ActionFileInto, ActionRedirect, ActionVacation, ActionPipe:
			return true
		case ActionKeep:
			return !x.Implicit
		}
		return false
	}
	for _, prev := range d.AppliedActions {
		if a, ok := action.(ActionPipe); ok {
			if b, ok := prev.(ActionPipe); ok && a.Program == b.Program {
				return fmt.Errorf("pipe may use a program only once")
			}
		}
		if (isReject(action) && (delivers(prev) || isReject(prev))) || (delivers(action) && isReject(prev)) {
			return fmt.Errorf("action conflict: %s and %s", prev.testActionName(), action.testActionName())
		}
		if _, ok := action.(ActionVacation); ok {
			if _, dup := prev.(ActionVacation); dup {
				return fmt.Errorf("vacation may be used only once")
			}
		}
	}
	return nil
}

func NewRuntimeData(s *Script, p PolicyReader, e Envelope, m Message) *RuntimeData {
	return &RuntimeData{
		Script:       s,
		Policy:       p,
		Envelope:     e,
		Msg:          m,
		OnAction:     DefaultOnAction,
		ImplicitKeep: true,
		FlagAliases:  make(map[string]string),
		Variables:    map[string]string{},
	}
}

// KeepWithoutPipes reports whether unsuccessful :try pipes must restore keep.
func (d *RuntimeData) KeepWithoutPipes() bool {
	if !d.ImplicitKeep {
		return false
	}
	for _, a := range d.AppliedActions {
		if _, pipe := a.(ActionPipe); !pipe && a.cancelsImplicitKeep() {
			return false
		}
	}
	return true
}

func (d *RuntimeData) executionContext() context.Context {
	if d.runContext != nil {
		return d.runContext
	}
	return context.Background()
}

func (d *RuntimeData) checkExecution() error {
	if err := d.executionContext().Err(); err != nil {
		return err
	}
	return d.mimeErr
}
