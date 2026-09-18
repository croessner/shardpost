package tests

import (
	"github.com/foxcpp/go-sieve/interp"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPigeonholeConformance runs every .svtest of the Pigeonhole test
// suite (checkout under pigeonhole/) and reports the outcome per file.
// Failures fail the build. Unsupported extensions and non-conformance
// crash/diagnostic fixtures are explicitly excluded and never counted as passes.
func TestPigeonholeConformance(t *testing.T) {
	if os.Getenv("SIEVE_CONFORMANCE") == "" {
		t.Skip("set SIEVE_CONFORMANCE=1")
	}
	var files []string
	_ = filepath.WalkDir("pigeonhole/tests", func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".svtest") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	pass, fail, skipped, excluded := 0, 0, 0, 0
	var failed []string
	for _, f := range files {
		ran, excludedFile := false, false
		ok := t.Run(strings.TrimPrefix(f, "pigeonhole/tests/"), func(t *testing.T) {
			ran = true
			name := strings.TrimPrefix(f, "pigeonhole/tests/")
			reason := conformanceExclusion(name)
			if reason != "" {
				excludedFile = true
				t.Skip(reason)
			}
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			env := &simpleExecuteRuntime{}
			RunDovecotTest(t, f, ExecuteTestRuntime(env), func(d *interp.RuntimeData) { d.MailboxExists = env.MailboxExists })
		})
		if excludedFile {
			excluded++
			continue
		}
		if !ran {
			skipped++
			continue
		}
		if ok {
			pass++
		} else {
			fail++
			failed = append(failed, strings.TrimPrefix(f, "pigeonhole/tests/"))
		}
	}
	t.Logf("SIEVE CONFORMANCE: %d passed, %d failed, %d excluded, %d not run of %d files", pass, fail, excluded, skipped, len(files))
	for _, f := range failed {
		t.Logf("  FAIL %s", f)
	}
}

func conformanceExclusion(name string) string {
	switch {
	case name == "extensions/extlists/basic.svtest" || name == "extensions/extlists/multiple.svtest" || name == "extensions/extlists/valid_ext_list.svtest":
		return "unsupported optional extension extlists; no list backend or advertised capability"
	case name == "extensions/vnd.dovecot/environment/basic.svtest" || name == "extensions/vnd.dovecot/environment/variables.svtest":
		return "unsupported Dovecot environment plugin; not advertised"
	case name == "extensions/vnd.dovecot/report/execute.svtest":
		return "unsupported Dovecot report plugin; not advertised"
	case name == "compile/errors.svtest":
		return "Dovecot-specific diagnostic texts/counts; all invalid-script acceptance checks run in TestCompileErrors"
	case name == "failures/fuzz1.svtest":
		return "historical crash input references a nonexistent mailbox/flag; checked separately as an error path, not a valid delivery scenario"
	case name == "failures/fuzz4.svtest":
		return "historical crash input has invalid notify syntax; rejection checked separately"
	}
	return ""
}
