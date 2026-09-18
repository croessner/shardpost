package interp

import (
	"context"
	"encoding/gob"
	"fmt"
	"strings"

	"github.com/foxcpp/go-sieve/lexer"
	"github.com/foxcpp/go-sieve/parser"
)

var supportedRequires = map[string]struct{}{
	"fileinto":          {},
	"envelope":          {},
	"encoded-character": {},

	"comparator-i;octet":           {},
	"comparator-i;ascii-casemap":   {},
	"comparator-i;ascii-numeric":   {},
	"comparator-i;unicode-casemap": {},

	"imap4flags":  {},
	"variables":   {},
	"relational":  {},
	"copy":        {},
	"reject":      {},
	"ereject":     {},
	"subaddress":  {},
	"environment": {},
	"body":        {},
	// RFC 5230 / RFC 6131 (shardpost fork)
	"vacation":         {},
	"vacation-seconds": {},
	"date":             {},
	"duplicate":        {},
	"editheader":       {},
	"mailbox":          {},
	// RFC 5463, 6609, 5260, 5703 (partly), 5235, 5490, 8579
	"ihave":          {},
	"include":        {},
	"index":          {},
	"regex":          {},
	"spamtest":       {},
	"spamtestplus":   {},
	"virustest":      {},
	"mboxmetadata":   {},
	"servermetadata": {},
	"special-use":    {},
	"enotify":        {},
	"foreverypart":   {},
	"mime":           {},
	"extracttext":    {},
	// Pigeonhole extension: debug_log writes to the log (here: the test log)
	"vnd.dovecot.debug": {},
	// external programs through the server's allowlist (RuntimeData.Program)
	"vnd.dovecot.execute": {},
	"vnd.dovecot.pipe":    {},
	"vnd.dovecot.filter":  {},
}

var (
	commands map[string]func(*Script, parser.Cmd) (Cmd, error)
	tests    map[string]func(*Script, parser.Test) (Test, error)
)

func init() {
	// break initialization loop

	commands = map[string]func(*Script, parser.Cmd) (Cmd, error){
		// RFC 5228
		"require":  loadRequire,
		"if":       loadIf,
		"elsif":    loadElsif,
		"else":     loadElse,
		"stop":     loadStop,
		"fileinto": loadFileInto, // fileinto extension
		"redirect": loadRedirect,
		"keep":     loadKeep,
		"discard":  loadDiscard,
		// RFC 5232 (imap4flags extension)
		"setflag":    loadSetFlag,
		"addflag":    loadAddFlag,
		"removeflag": loadRemoveFlag,
		// RFC 5229 (variables extension)
		"set": loadSet,
		// RFC 5429 (reject/ereject extensions)
		"reject":  loadReject,
		"ereject": loadEReject,
		// RFC 5230 (vacation extension)
		"vacation": loadVacation,
		// RFC 5293 (editheader extension)
		"addheader":    loadAddHeader,
		"deleteheader": loadDeleteHeader,
		// RFC 5463 (ihave), RFC 6609 (include)
		"error":   loadErrorCmd,
		"include": loadInclude,
		"global":  loadGlobal,
		"return":  loadReturn,
		// vnd.dovecot.debug
		"debug_log": loadDebugLog,
		// RFC 5435 (enotify)
		"notify": loadNotify,
		// vnd.dovecot.execute / pipe / filter
		"execute": programCmdLoader(ProgramExecute),
		"pipe":    programCmdLoader(ProgramPipe),
		"filter":  programCmdLoader(ProgramFilter),
		// RFC 5703 (foreverypart, extracttext)
		"foreverypart": loadForEveryPart,
		"break":        loadBreak,
		"extracttext":  loadExtractText,
		// vnd.dovecot.testsuite
		"test":                loadDovecotTest,
		"test_set":            loadDovecotTestSet,
		"test_fail":           loadDovecotTestFail,
		"test_binary_load":    loadDovecotBinaryLoad,
		"test_binary_save":    loadDovecotBinarySave,
		"test_mailbox_create": loadDovecotMailboxCreate,
		// "test_imap_metadata_set"
		"test_imap_metadata_set": loadDovecotMetadataSet,
		"test_config_reload":     loadNoop, // go-sieve applies changes immediately
		"test_config_set":        loadDovecotConfigSet,
		"test_config_unset":      loadDovecotConfigUnset,
		"test_result_print":      loadDovecotResultPrint,
		"test_result_reset":      loadDovecotResultReset,
		"test_message":           loadDovecotCmdMessage,
	}
	tests = map[string]func(*Script, parser.Test) (Test, error){
		// RFC 5228
		"address":  loadAddressTest,
		"allof":    loadAllOfTest,
		"anyof":    loadAnyOfTest,
		"envelope": loadEnvelopeTest, // envelope extension
		"exists":   loadExistsTest,
		"false":    loadFalseTest,
		"true":     loadTrueTest,
		"header":   loadHeaderTest,
		"not":      loadNotTest,
		"size":     loadSizeTest,
		// RFC 5229 (variables extension)
		"string": loadStringTest,
		// RFC 5232 (imap4flags extension)
		"hasflag": loadHasFlagTest,
		// RFC 5183 (environment extension)
		"environment": loadEnvironmentTest,
		// RFC 5173 (body extension)
		"body": loadBodyTest,
		// RFC 5260 (date extension)
		"date":        loadDateTest,
		"currentdate": loadCurrentDateTest,
		// RFC 7352 (duplicate extension)
		"duplicate": loadDuplicateTest,
		// RFC 5490 (mailbox extension)
		"mailboxexists": loadMailboxExistsTest,
		// RFC 5463 (ihave), RFC 5235 (spamtest/virustest), RFC 5490 (metadata), RFC 8579
		"ihave":                    loadIhaveTest,
		"spamtest":                 loadSpamTest,
		"virustest":                loadVirusTest,
		"metadata":                 loadMetadataTest,
		"metadataexists":           loadMetadataExistsTest,
		"servermetadata":           loadServerMetadataTest,
		"servermetadataexists":     loadServerMetadataExistsTest,
		"specialuse_exists":        loadSpecialUseExistsTest,
		"execute":                  programTestLoader(ProgramExecute),
		"pipe":                     programTestLoader(ProgramPipe),
		"filter":                   programTestLoader(ProgramFilter),
		"valid_notify_method":      loadValidNotifyMethodTest,
		"notify_method_capability": loadNotifyMethodCapabilityTest,
		// vnd.dovecot.testsuite
		"test_script_compile": loadDovecotCompile, // compile script (to test for compile errors)
		"test_multiscript":    loadDovecotMultiscript,
		"test_script_run":     loadDovecotRun,           // run script (to test for run-time errors)
		"test_error":          loadDovecotError,         // check detailed results of test_script_compile or test_script_run
		"test_message":        loadDovecotTestMessage,   // check results of test_result_execute - where messages are
		"test_result_action":  loadDovecotResultAction,  // check results of test_result_execute - what actions are executed
		"test_result_execute": loadDovecotResultExecute, // apply script results (validated using test_message)
	}
}

func LoadScript(cmdStream []parser.Cmd, opts *Options) (*Script, error) {
	s := &Script{
		extensions: map[string]struct{}{},
		opts:       opts,
	}

	loadedCmds, err := LoadBlock(s, cmdStream)
	if err != nil {
		return nil, err
	}
	if s.RequiresExtension("extracttext") && !s.RequiresExtension("foreverypart") {
		return nil, fmt.Errorf("extracttext requires foreverypart")
	}
	s.cmd = loadedCmds

	return s, nil
}

func LoadBlock(s *Script, cmds []parser.Cmd) ([]Cmd, error) {
	loaded := make([]Cmd, 0, len(cmds))
	for _, c := range cmds {
		cmd, err := LoadCmd(s, c)
		if err != nil {
			return nil, fmt.Errorf("LoadCmd %s: %w", c.Id, err)
		}
		if cmd == nil {
			continue
		}
		loaded = append(loaded, cmd)
	}
	return loaded, nil
}

func LoadCmd(s *Script, cmd parser.Cmd) (Cmd, error) {
	cmdName := strings.ToLower(cmd.Id)
	if s.opts.Conf("imapsieve_context") == "yes" {
		switch cmdName {
		case "vacation", "reject", "ereject":
			return nil, fmt.Errorf("action unavailable in IMAP context: %s", cmdName)
		}
	}
	factory := commands[cmdName]
	if factory == nil {
		return nil, lexer.ErrorAt(cmd, "LoadBlock: unsupported command: %v", cmdName)
	}
	return factory(s, cmd)

}

func LoadTest(s *Script, t parser.Test) (Test, error) {
	testName := strings.ToLower(t.Id)
	factory := tests[testName]
	if factory == nil {
		return nil, lexer.ErrorAt(t, "LoadTest: unsupported test: %v", testName)
	}
	return factory(s, t)
}

type CmdNoop struct{}

func (c CmdNoop) Execute(_ context.Context, _ *RuntimeData) error {
	return nil
}

func loadNoop(_ *Script, _ parser.Cmd) (Cmd, error) {
	return CmdNoop{}, nil
}

func init() { gob.Register(CmdNoop{}) }
