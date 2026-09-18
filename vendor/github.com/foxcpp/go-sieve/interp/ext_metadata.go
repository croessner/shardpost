package interp

import (
	"context"
	"encoding/gob"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/foxcpp/go-sieve/parser"
)

// mboxmetadata / servermetadata (RFC 5490 §3, §4) and special-use
// (RFC 8579), shardpost fork.

func validAnnotation(a string) bool {
	return (strings.HasPrefix(a, "/private/") || strings.HasPrefix(a, "/shared/")) && !strings.Contains(a, "//") && !strings.HasSuffix(a, "/")
}

// MetadataTest is metadata / servermetadata (Server=true).
type MetadataTest struct {
	matcherTest
	Server     bool
	Mailbox    string
	Annotation string
}

func (t MetadataTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	mailbox := ""
	if !t.Server {
		mailbox = expandVars(d, t.Mailbox)
		if !utf8.ValidString(mailbox) {
			return false, nil
		}
	}
	ann := expandVars(d, t.Annotation)
	if !validAnnotation(ann) || d.Metadata == nil {
		return false, nil
	}
	value, ok, err := d.Metadata(ctx, mailbox, ann)
	if err != nil {
		return false, err
	}
	if t.isCount() {
		n := uint64(0)
		if ok {
			n = 1
		}
		return t.countMatches(d, n), nil
	}
	if !ok {
		return false, nil
	}
	return t.tryMatch(d, value)
}

// MetadataExistsTest is metadataexists / servermetadataexists.
type MetadataExistsTest struct {
	Server      bool
	Mailbox     string
	Annotations []string
}

func (t MetadataExistsTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	mailbox := ""
	if !t.Server {
		mailbox = expandVars(d, t.Mailbox)
		if !utf8.ValidString(mailbox) {
			return false, nil
		}
	}
	if d.Metadata == nil {
		return false, nil
	}
	for _, a := range t.Annotations {
		a = expandVars(d, a)
		if !validAnnotation(a) {
			return false, nil
		}
		if _, ok, err := d.Metadata(ctx, mailbox, a); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func loadMetadataTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("mboxmetadata") {
		return nil, parser.ErrorAt(test.Position, "missing require 'mboxmetadata'")
	}
	loaded := MetadataTest{matcherTest: newMatcherTest()}
	var key []string
	spec := loaded.addSpecTags(&Spec{Pos: []SpecPosArg{
		{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Mailbox = v[0] }},
		{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Annotation = v[0] }},
		{MinStrCount: 1, MatchStr: func(v []string) { key = v }},
	}})
	if err := LoadSpec(s, spec, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, nil
}

func loadServerMetadataTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("servermetadata") {
		return nil, parser.ErrorAt(test.Position, "missing require 'servermetadata'")
	}
	loaded := MetadataTest{matcherTest: newMatcherTest(), Server: true}
	var key []string
	spec := loaded.addSpecTags(&Spec{Pos: []SpecPosArg{
		{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Annotation = v[0] }},
		{MinStrCount: 1, MatchStr: func(v []string) { key = v }},
	}})
	if err := LoadSpec(s, spec, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, nil
}

func loadMetadataExistsTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("mboxmetadata") {
		return nil, parser.ErrorAt(test.Position, "missing require 'mboxmetadata'")
	}
	loaded := MetadataExistsTest{}
	err := LoadSpec(s, &Spec{Pos: []SpecPosArg{
		{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Mailbox = v[0] }},
		{MinStrCount: 1, MatchStr: func(v []string) { loaded.Annotations = v }},
	}}, test.Position, test.Args, test.Tests, nil)
	return loaded, err
}

func loadServerMetadataExistsTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("servermetadata") {
		return nil, parser.ErrorAt(test.Position, "missing require 'servermetadata'")
	}
	loaded := MetadataExistsTest{Server: true}
	err := LoadSpec(s, &Spec{Pos: []SpecPosArg{
		{MinStrCount: 1, MatchStr: func(v []string) { loaded.Annotations = v }},
	}}, test.Position, test.Args, test.Tests, nil)
	return loaded, err
}

// CmdDovecotMetadataSet is the testsuite's test_imap_metadata_set.
type CmdDovecotMetadataSet struct {
	Mailbox    string
	Annotation string
	Value      string
}

func (c CmdDovecotMetadataSet) Execute(_ context.Context, d *RuntimeData) error {
	if d.Test == nil {
		return fmt.Errorf("test runtime is not configured")
	}
	if d.Test.Metadata == nil {
		d.Test.Metadata = map[string]string{}
	}
	d.Test.Metadata[c.Mailbox+"\x00"+c.Annotation] = expandVars(d, c.Value)
	return nil
}

func loadDovecotMetadataSet(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension(DovecotTestExtension) || s.opts.T == nil {
		return nil, fmt.Errorf("testing environment is not enabled")
	}
	cmd := CmdDovecotMetadataSet{}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{"mailbox": {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Mailbox = v[0] }}},
		Pos: []SpecPosArg{
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Annotation = v[0] }},
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Value = v[0] }},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	return cmd, err
}

// TestMetadata reads the annotations the testsuite set with
// test_imap_metadata_set (for RuntimeData.Metadata in test runs).
func TestMetadata(d *RuntimeData) func(context.Context, string, string) (string, bool, error) {
	return func(_ context.Context, mailbox, annotation string) (string, bool, error) {
		if d.Test == nil {
			return "", false, nil
		}
		v, ok := d.Test.Metadata[mailbox+"\x00"+annotation]
		return v, ok, nil
	}
}

// --- special-use -----------------------------------------------------------

// SpecialUseExistsTest is specialuse_exists [mailbox] uses (RFC 8579 3).
type SpecialUseExistsTest struct {
	Mailbox string
	Uses    []string
}

func (t SpecialUseExistsTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	if d.SpecialUseExists == nil {
		return false, nil
	}
	mailbox := expandVars(d, t.Mailbox)
	if !utf8.ValidString(mailbox) {
		return false, nil
	}
	uses := expandVarsList(d, t.Uses)
	for _, u := range uses {
		if !validSpecialUse(u) {
			return false, nil
		}
	}
	return d.SpecialUseExists(ctx, mailbox, uses)
}

func validSpecialUse(u string) bool {
	if len(u) < 2 || u[0] != '\\' {
		return false
	}
	for _, r := range u[1:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func loadSpecialUseExistsTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("special-use") {
		return nil, parser.ErrorAt(test.Position, "missing require 'special-use'")
	}
	loaded := SpecialUseExistsTest{}
	var a1, a2 []string
	if err := LoadSpec(s, &Spec{Pos: []SpecPosArg{
		{MinStrCount: 1, MatchStr: func(v []string) { a1 = v }},
		{MinStrCount: 1, Optional: true, MatchStr: func(v []string) { a2 = v }},
	}}, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if len(a2) == 0 {
		loaded.Uses = a1
	} else {
		if len(a1) != 1 {
			return nil, parser.ErrorAt(test.Position, "specialuse_exists: one mailbox name expected")
		}
		loaded.Mailbox, loaded.Uses = a1[0], a2
	}
	for _, u := range loaded.Uses {
		if len(usedVars(s, u)) == 0 && !validSpecialUse(u) {
			return nil, parser.ErrorAt(test.Position, "specialuse_exists: invalid special-use flag %q", u)
		}
	}
	return loaded, nil
}

func init() {
	gob.Register(MetadataTest{})
	gob.Register(MetadataExistsTest{})
	gob.Register(CmdDovecotMetadataSet{})
	gob.Register(SpecialUseExistsTest{})
}
