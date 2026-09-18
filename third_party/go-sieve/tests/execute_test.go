package tests

import (
	"testing"
)

func TestExecute(t *testing.T) {
	RunExecuteTests(t, &simpleExecuteRuntime{})
}
