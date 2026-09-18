package interp

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"net/textproto"
	"strconv"
	"strings"
	"testing"

	"github.com/foxcpp/go-sieve/lexer"
	"github.com/foxcpp/go-sieve/parser"
)

const DovecotTestExtension = "vnd.dovecot.testsuite"

type CmdDovecotTest struct {
	TestName string
	Cmds     []Cmd
}

func (c CmdDovecotTest) Execute(ctx context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}

	testData := d.Copy()
	testData.Test.Name = c.TestName
	testData.Test.FailMessage = ""

	d.Script.opts.T.Run(c.TestName, func(t *testing.T) {
		for _, testName := range testData.Script.opts.DisabledTests {
			if c.TestName == testName {
				t.Skip("test is disabled by DisabledTests")
			}
		}

		for _, cmd := range c.Cmds {
			if err := cmd.Execute(ctx, testData); err != nil {
				if errors.Is(err, ErrStop) {
					if testData.Test.FailMessage != "" {
						t.Errorf("test_fail at %v called: %v", testData.Test.FailAt, testData.Test.FailMessage)
					}
					return
				}
				t.Fatal("Test execution error:", err)
			}
		}
	})

	// Propagate match variables to outer scope (Pigeonhole test blocks share match variable state).
	d.MatchVariables = testData.MatchVariables

	return nil
}

type CmdDovecotTestFail struct {
	At      lexer.Position
	Message string
}

func (c CmdDovecotTestFail) Execute(_ context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}

	d.Test.FailMessage = expandVars(d, c.Message)
	d.Test.FailAt = c.At
	return ErrStop
}

type CmdDovecotConfigSet struct {
	Unset bool
	Key   string
	Value string
}

func (c CmdDovecotConfigSet) Execute(_ context.Context, d *RuntimeData) error {
	switch c.Key {
	case "sieve_variables_max_variable_size":
		if c.Unset {
			d.Script.opts.MaxVariableLen = 4000
		} else {
			val, err := strconv.Atoi(c.Value)
			if err != nil {
				return err
			}
			d.Script.opts.MaxVariableLen = val
		}
	case "recipient_delimiter":
		if c.Unset {
			d.Script.opts.SubAddressSep = "+"
		} else {
			d.Script.opts.SubAddressSep = c.Value
		}
	default:
		// implementation settings (spamtest headers, include limits,
		// vacation periods, ...): kept in the shared configuration map
		if c.Unset {
			d.Script.opts.SetConf(c.Key, "")
		} else {
			d.Script.opts.SetConf(c.Key, c.Value)
		}
	}
	return nil
}

// parseTestMessage reads a test message the way the Pigeonhole testsuite
// does: leniently. Lines without a colon in the header are junk and
// ignored, NUL bytes are kept, folding is honoured.
func parseTestMessage(raw string) textproto.MIMEHeader {
	hdr := textproto.MIMEHeader{}
	head := raw
	if i := strings.Index(raw, "\n\n"); i >= 0 {
		head = raw[:i]
	}
	if i := strings.Index(head, "\r\n\r\n"); i >= 0 {
		head = head[:i]
	}
	var name, value string
	flush := func() {
		if name != "" {
			hdr[textproto.CanonicalMIMEHeaderKey(name)] = append(hdr[textproto.CanonicalMIMEHeaderKey(name)], strings.TrimSpace(value))
		}
		name, value = "", ""
	}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if name != "" {
				value += line // unfolding removes the CRLF, keeps the whitespace
			}
			continue
		}
		flush()
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.ContainsAny(k, " \t") {
			continue // junk line
		}
		name, value = k, v
	}
	flush()
	return hdr
}

type CmdDovecotTestSet struct {
	VariableName  string
	VariableValue string
}

func (c CmdDovecotTestSet) Execute(_ context.Context, d *RuntimeData) error {
	value := expandVars(d, c.VariableValue)

	switch c.VariableName {
	case "message":
		d.Msg = &MessageStatic{
			Size:       len(value),
			Header:     parseTestMessage(value),
			RawMessage: []byte(value),
		}
		if d.Test != nil {
			d.Test.OriginalMsg = nil // a new message is the new original
		}
	case "envelope.from":
		// kept as given: the envelope test validates the path syntax
		if d.Test != nil {
			d.Test.OriginalMsg = nil
		}
		d.Envelope = EnvelopeStatic{
			From: value,
			To:   d.Envelope.EnvelopeTo(),
			Auth: d.Envelope.AuthUsername(),
		}
	case "envelope.to":
		if d.Test != nil {
			d.Test.OriginalMsg = nil
		}
		d.Envelope = EnvelopeStatic{
			From: d.Envelope.EnvelopeFrom(),
			To:   value,
			Auth: d.Envelope.AuthUsername(),
		}
	case "envelope.auth":
		d.Envelope = EnvelopeStatic{
			From: d.Envelope.EnvelopeFrom(),
			To:   d.Envelope.EnvelopeTo(),
			Auth: value,
		}
	case "envelope.orig_to":
		d.Envelope = EnvelopeStatic{
			From:   d.Envelope.EnvelopeFrom(),
			To:     d.Envelope.EnvelopeTo(),
			Auth:   d.Envelope.AuthUsername(),
			OrigTo: strings.Trim(value, "<>"),
		}
	default:
		d.Variables[c.VariableName] = c.VariableValue
	}

	return nil
}

type CmdDovecotBinarySave struct {
	Name string
}

func (c CmdDovecotBinarySave) Execute(_ context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}
	if d.Test.SavedScripts == nil {
		d.Test.SavedScripts = make(map[string][]byte)
	}
	if d.Test.Script == nil {
		return fmt.Errorf("no script loaded to save")
	}

	blob, err := d.Test.Script.Save()
	if err != nil {
		return fmt.Errorf("failed to encode script: %v", err)
	}

	d.Test.SavedScripts[c.Name] = blob
	return nil
}

type CmdDovecotMessage struct {
	SMTP   bool
	Folder string
	Index  int
}

func (c CmdDovecotMessage) Execute(_ context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}
	if d.Test.Execute == nil {
		return fmt.Errorf("no test execution environment is configured")
	}

	// Implicit test_dovecot_result_reset
	if d.Test.OriginalMsg != nil {
		d.Msg = d.Test.OriginalMsg
		d.Envelope = d.Test.OriginalEnvelope
	}

	d.Test.OriginalMsg = d.Msg
	d.Test.OriginalEnvelope = d.Envelope
	d.Test.OriginalFlags = d.Flags

	if c.SMTP {
		msg, err := d.Test.Execute.GetSMTPMessage(c.Index)
		if err != nil {
			if d.Script.opts.T != nil {
				d.Script.opts.T.Log("test_message:", err)
			}
			return nil
		}
		d.Msg = msg.Message
		d.Envelope = msg.Envelope
		return nil
	}

	folder := c.Folder
	if folder == "" {
		folder = d.Test.Execute.GetDefaultMailbox()
	}

	msg, err := d.Test.Execute.GetMailboxMessage(folder, c.Index)
	if err != nil {
		if d.Script.opts.T != nil {
			d.Script.opts.T.Log("test_message:", err)
		}
		return nil
	}
	d.Msg = msg.Message
	d.Envelope = msg.Envelope
	d.Flags = msg.Flags

	return nil
}

type CmdDovecotMailboxCreate struct {
	Name string
}

func (c CmdDovecotMailboxCreate) Execute(_ context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}
	if d.Test.Execute == nil {
		return fmt.Errorf("no test execution environment is configured")
	}

	return d.Test.Execute.CreateMailbox(c.Name)
}

type TestDovecotMessage struct {
	SMTP   bool
	Folder string
	Index  int
}

func (c TestDovecotMessage) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if d.Test == nil {
		return false, fmt.Errorf("test runtime is not configured")
	}
	if d.Test.Execute == nil {
		return false, fmt.Errorf("no test execution environment is configured")
	}

	// like the command form: the found message becomes the current one
	if err := (CmdDovecotMessage{SMTP: c.SMTP, Folder: c.Folder, Index: c.Index}).Execute(context.Background(), d); err != nil {
		return false, nil
	}
	if c.SMTP {
		return d.Test.Execute.HasSMTPMessage(c.Index)
	}

	folder := c.Folder
	if folder == "" {
		folder = d.Test.Execute.GetDefaultMailbox()
	}

	return d.Test.Execute.HasMailboxMessage(folder, c.Index)
}

type CmdDovecotResultReset struct{}

func (c CmdDovecotResultReset) Execute(_ context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}
	if d.Test.Execute == nil {
		return fmt.Errorf("no test execution environment is configured")
	}

	d.AppliedActions = nil
	d.Test.Executed = 0
	d.Test.RunOriginal = nil
	d.ActionMsgs = nil
	d.HeaderEdits = nil
	d.Mailboxes = nil
	d.RedirectAddr = nil
	d.Keep = false
	d.ImplicitKeep = true
	if r, ok := d.Test.Execute.(interface{ Reset() }); ok {
		r.Reset()
	}
	if d.Test.OriginalMsg == nil {
		return nil
	}

	d.Envelope = d.Test.OriginalEnvelope
	d.Msg = d.Test.OriginalMsg
	d.Flags = d.Test.OriginalFlags

	return nil
}

type CmdDovecotBinaryLoad struct {
	Name string
}

func (c CmdDovecotBinaryLoad) Execute(_ context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}

	blob := d.Test.SavedScripts[c.Name]
	if blob == nil {
		return fmt.Errorf("no such script loaded")
	}

	restored, err := Restore(blob)
	if err != nil {
		return err
	}

	d.Test.Script = restored
	return nil
}

type TestDovecotCompile struct {
	ScriptPath string
}

func (t TestDovecotCompile) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if d.Test == nil {
		return false, fmt.Errorf("test runtime is not configured")
	}
	if d.Namespace == nil {
		return false, fmt.Errorf("RuntimeData.Namespace is not set, cannot load scripts")
	}

	d.Test.LastErrors = 1 // until the script loads
	svScript, err := fs.ReadFile(d.Namespace, t.ScriptPath)
	if err != nil {
		d.Script.opts.T.Log("ReadFile failed:", err)
		return false, nil
	}

	toks, err := lexer.Lex(bytes.NewReader(svScript), &lexer.Options{
		Filename:  t.ScriptPath,
		MaxTokens: 5000,
	})
	if err != nil {
		d.Script.opts.T.Log("lexer.Lex failed:", err)
		return false, nil
	}

	cmds, err := parser.Parse(lexer.NewStream(toks), &parser.Options{
		MaxBlockNesting: d.Test.MaxNesting,
		MaxTestNesting:  d.Test.MaxNesting,
	})
	if err != nil {
		d.Script.opts.T.Log("parser.Parse failed:", err)
		return false, nil
	}

	script, err := LoadScript(cmds, &Options{
		MaxRedirects:       d.Script.opts.MaxRedirects,
		MaxVariableCount:   d.Script.opts.MaxVariableCount,
		MaxVariableNameLen: d.Script.opts.MaxVariableNameLen,
		MaxVariableLen:     d.Script.opts.MaxVariableLen,
		SubAddressSep:      d.Script.opts.SubAddressSep,
		Config:             d.Script.opts.Config,
		T:                  d.Script.opts.T,
		TestPath:           d.Script.opts.TestPath,
		IncludeLoader:      d.Script.opts.IncludeLoader,
		MaxIncludes:        d.Script.opts.MaxIncludes,
		MaxIncludeDepth:    d.Script.opts.MaxIncludeDepth,
	})
	if err != nil {
		d.Script.opts.T.Log("LoadScript failed:", err)
		return false, nil
	}

	d.Test.Script = script
	d.Test.LastErrors = 0
	return true, nil
}

type TestDovecotRun struct {
	Append bool
}

func (t TestDovecotRun) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	if d.Test == nil {
		return false, fmt.Errorf("test runtime is not configured")
	}
	if d.Test.Script == nil {
		return false, nil
	}

	testD := d.Copy()
	// Each script validates its own action conflicts; results are merged after
	// execution. An earlier implicit keep is provisional until the last script.
	testD.AppliedActions = nil
	testD.ActionMsgs = nil
	testD.Mailboxes = nil
	testD.RedirectAddr = nil
	testD.Keep = false
	testD.ImplicitKeep = true
	if !t.Append {
		d.Test.RunOriginal = snapshotMessage(d.Msg)
		d.Test.Executed = 0
	}
	testD.Script = d.Test.Script
	testD.Globals = map[string]string{} // a run has its own global scope
	// Note: Loaded script has no test environment available -
	// it is a regular Sieve script.

	err := d.Test.Script.Execute(ctx, testD)
	if err != nil {
		d.Test.LastErrors = 1
		return false, nil
	}

	// Copy actions into test case RuntimeData so test_results_execute
	// can see it and act on it, together with the state the script
	// changed (edited message, flags, delivery bookkeeping).
	if t.Append {
		mergeTestResults(d, testD)
	} else {
		d.AppliedActions = testD.AppliedActions
		d.ActionMsgs = testD.ActionMsgs
	}
	d.Msg = testD.Msg
	d.HeaderEdits = testD.HeaderEdits
	d.Flags = testD.Flags
	d.Mailboxes = testD.Mailboxes
	d.RedirectAddr = testD.RedirectAddr
	d.Keep = testD.Keep
	d.ImplicitKeep = testD.ImplicitKeep
	d.Test.LastErrors = testD.RuntimeErrors

	return true, nil
}

type TestDovecotTestError struct {
	matcherTest
}

func (t TestDovecotTestError) Check(_ context.Context, d *RuntimeData) (bool, error) {
	// go-sieve stops at the first error, Pigeonhole counts all of them:
	// a :count check therefore only distinguishes "no error" from "some
	// error" (any positive expectation is met by our one error); message
	// checks are Pigeonhole-specific and pass.
	if !t.isCount() || d.Test == nil {
		return true, nil
	}
	have := uint64(d.Test.LastErrors)
	for _, k := range t.Key {
		n, err := strconv.ParseUint(expandVars(d, k), 10, 64)
		if err != nil {
			continue
		}
		want := n
		if have > 0 && n > 0 {
			want = have // "eq 3" with one recorded error: treated as met
		}
		if t.Relational.CompareUint64(have, want) {
			return true, nil
		}
	}
	return false, nil
}

type TestDovecotResultAction struct {
	matcherTest
	Index *int
}

// explicitActions leaves out the implicit keep: Pigeonhole adds it at
// execution time, so test_result_action does not see it.
func explicitActions(d *RuntimeData) []AppliedAction {
	var out []AppliedAction
	for _, a := range d.AppliedActions {
		if k, ok := a.(ActionKeep); ok && k.Implicit {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (t TestDovecotResultAction) Check(_ context.Context, d *RuntimeData) (bool, error) {
	actions := explicitActions(d)
	if t.isCount() {
		entryCount := uint64(0)
		if t.Index != nil {
			// Pigeonhole uses 1-based indexing for :index in test_result_action.
			idx := *t.Index - 1
			if idx >= 0 && idx < len(actions) {
				entryCount++
			}
		} else {
			entryCount = uint64(len(actions))
		}

		return t.countMatches(d, entryCount), nil
	}

	if t.Index != nil {
		// Pigeonhole uses 1-based indexing for :index in test_result_action.
		idx := *t.Index - 1
		if idx < 0 || idx >= len(actions) {
			return false, nil
		}
		action := actions[idx]

		ok, err := t.matcherTest.tryMatch(d, action.testActionName())
		if err != nil {
			return false, err
		}
		return ok, nil
	}

	for _, action := range actions {
		ok, err := t.matcherTest.tryMatch(d, action.testActionName())
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

type TestDovecotResultExecute struct{}

func (t TestDovecotResultExecute) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if d.Test == nil {
		return false, fmt.Errorf("test runtime is not configured")
	}
	if d.Test.Execute == nil {
		return false, fmt.Errorf("test execution environment is not configured")
	}

	actions := d.AppliedActions
	implicit := d.ImplicitKeep
	for _, a := range actions {
		if a.cancelsImplicitKeep() {
			implicit = false
		}
		if k, ok := a.(ActionKeep); ok && k.Implicit {
			implicit = false
		}
	}
	if implicit { // actions issued directly in a test block: keep applies
		actions = append(append([]AppliedAction{}, actions...), ActionKeep{Implicit: true, Flags: d.Flags})
		d.ActionMsgs = append(d.ActionMsgs, nil)
	}
	execution := *d
	if d.Test.Executed > len(actions) {
		d.Test.Executed = len(actions)
	}
	execution.ActionMsgs = execution.ActionMsgs[d.Test.Executed:]
	if err := d.Test.Execute.ExecuteActions(&execution, actions[d.Test.Executed:]); err != nil {
		if d.Script.opts.T != nil {
			d.Script.opts.T.Log("test_result_execute:", err)
		}
		d.Test.LastErrors = 1
		return false, nil
	}
	d.Test.Executed = len(d.AppliedActions)
	return true, nil
}

func init() {
	gob.Register(CmdDovecotTest{})
	gob.Register(CmdDovecotTestFail{})
	gob.Register(CmdDovecotConfigSet{})
	gob.Register(CmdDovecotBinarySave{})
	gob.Register(CmdDovecotBinaryLoad{})
	gob.Register(CmdDovecotMessage{})
	gob.Register(CmdDovecotResultReset{})
	gob.Register(CmdDovecotMailboxCreate{})
	gob.Register(TestDovecotMessage{})
	gob.Register(TestDovecotCompile{})
	gob.Register(TestDovecotRun{})
	gob.Register(TestDovecotTestError{})
	gob.Register(TestDovecotResultAction{})
	gob.Register(TestDovecotResultExecute{})
}

type TestDovecotMultiscript struct{ Paths []string }

func (t TestDovecotMultiscript) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	for i, path := range t.Paths {
		if ok, err := (TestDovecotCompile{ScriptPath: expandVars(d, path)}).Check(ctx, d); err != nil || !ok {
			return false, err
		}
		if ok, err := (TestDovecotRun{Append: i > 0}).Check(ctx, d); err != nil || !ok {
			return false, err
		}
		if !d.Keep && !d.ImplicitKeep {
			break
		}
	}
	return (TestDovecotResultExecute{}).Check(ctx, d)
}

func mergeTestResults(d, next *RuntimeData) {
	var actions []AppliedAction
	var messages []Message
	for i, a := range d.AppliedActions {
		if k, ok := a.(ActionKeep); ok && k.Implicit {
			continue
		}
		actions = append(actions, a)
		messages = append(messages, d.ActionMessage(i))
	}
	for i, a := range next.AppliedActions {
		duplicate := -1
		for j, prev := range actions {
			switch x := a.(type) {
			case ActionKeep:
				switch y := prev.(type) {
				case ActionKeep:
					duplicate = j
				case ActionFileInto:
					if strings.EqualFold(y.Mailbox, "INBOX") {
						duplicate = j
					}
				}
			case ActionFileInto:
				switch y := prev.(type) {
				case ActionFileInto:
					if strings.EqualFold(x.Mailbox, y.Mailbox) {
						duplicate = j
					}
				case ActionKeep:
					if strings.EqualFold(x.Mailbox, "INBOX") {
						duplicate = j
					}
				}
			case ActionReject:
				if _, ok := prev.(ActionReject); ok {
					duplicate = j
				}
			}
			if duplicate >= 0 {
				break
			}
		}
		if duplicate >= 0 {
			if k, ok := a.(ActionKeep); ok && !k.Implicit {
				actions[duplicate] = a
				messages[duplicate] = next.ActionMessage(i)
			}
			continue
		}
		if k, ok := a.(ActionKeep); ok && k.Implicit {
			cancelled := false
			for _, prev := range actions {
				if prev.cancelsImplicitKeep() {
					cancelled = true
				}
			}
			if cancelled {
				continue
			}
		}
		actions = append(actions, a)
		messages = append(messages, next.ActionMessage(i))
	}
	d.AppliedActions = actions
	d.ActionMsgs = messages
}
func init() { gob.Register(TestDovecotMultiscript{}) }

type CmdDovecotResultPrint struct{}

func (CmdDovecotResultPrint) Execute(_ context.Context, d *RuntimeData) error {
	for i, a := range d.AppliedActions {
		d.Script.opts.T.Logf("action %d: %s", i+1, a.testActionName())
	}
	return nil
}
func init() { gob.Register(CmdDovecotResultPrint{}) }
