package interp

import (
	"context"
	"encoding/gob"
	"regexp"
	"strconv"
	"strings"
)

/*
variable-ref        =  "${" [namespace] variable-name "}"
namespace           =  identifier "." *sub-namespace
sub-namespace       =  variable-name "."
variable-name       =  num-variable / identifier
num-variable        =  1*DIGIT
*/
var variableRegexp = regexp.MustCompile(`\${(?:[a-zA-Z_][a-zA-Z0-9_]*\.(?:(?:[a-zA-Z_][a-zA-Z0-9_]*|[0-9]+)\.)*)?(?:[a-zA-Z_][a-zA-Z0-9_]*|[0-9]+)}`)

func usedVars(script *Script, s string) []string {
	if !script.RequiresExtension("variables") {
		return nil
	}

	variables := variableRegexp.FindAllString(s, -1)
	for i := range variables {
		// Cut ${} and case-fold.
		variables[i] = strings.ToLower(variables[i][2 : len(variables[i])-1])
	}

	return variables
}

func usedVarsAreValid(script *Script, s string) bool {
	for _, v := range usedVars(script, s) {
		matchNum, err := strconv.Atoi(v)
		if err == nil && matchNum >= 0 {
			continue
		}

		_, gettable := script.IsVarUsable(v)
		if !gettable {
			return false
		}
	}
	return true
}

func expandVarsList(d *RuntimeData, list []string) []string {
	if !d.Script.RequiresExtension("variables") {
		return list
	}

	listCpy := make([]string, len(list))
	for i, val := range list {
		listCpy[i] = expandVars(d, val)
	}
	return listCpy
}

func expandVars(d *RuntimeData, s string) string {
	if !d.Script.RequiresExtension("variables") {
		return s
	}

	expanded := variableRegexp.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]

		if matchNum, err := strconv.Atoi(name); err == nil && matchNum >= 0 {
			return d.MatchVariable(matchNum)
		}

		value, err := d.Var(name)
		if err != nil {
			panic("attempt to use an unusable variable: " + name)
		}
		return value
	})
	return expanded
}

type CmdSet struct {
	Name  string
	Value string
	// Modifiers are the :lower/:upper/:lowerfirst/:upperfirst/
	// :quotewildcard/:length tags; they are applied by precedence
	// (RFC 5229 4.1) when the command runs.
	Modifiers []string
}

func (c CmdSet) Execute(_ context.Context, d *RuntimeData) error {
	return d.SetVar(c.Name, applySetModifiers(d.Script.opts.MaxVariableLen, c.Modifiers, expandVars(d, c.Value)))
}

func modifierPrecedence(name string) int {
	switch name {
	case "length":
		return 10
	case "quotewildcard":
		return 20
	case "encodeurl":
		return 15
	case "upperfirst", "lowerfirst":
		return 30
	case "upper", "lower":
		return 40
	}
	return 0
}

func applyModifier(name, s string) string {
	switch name {
	case "length":
		return strconv.Itoa(len([]rune(s))) // characters, not octets
	case "quotewildcard":
		escaped := strings.Builder{}
		escaped.Grow(len(s))
		for _, chr := range s {
			switch chr {
			case '\\', '*', '?':
				escaped.WriteByte('\\')
				escaped.WriteRune(chr)
			default:
				escaped.WriteRune(chr)
			}
		}
		return escaped.String()
	case "encodeurl":
		return EncodeURL(s)
	case "upper":
		return strings.ToUpper(s)
	case "lower":
		return strings.ToLower(s)
	case "upperfirst", "lowerfirst":
		if len(s) == 0 {
			return s
		}
		first := s[0]
		if name == "upperfirst" && first >= 'a' && first <= 'z' {
			first -= 'a' - 'A'
		}
		if name == "lowerfirst" && first >= 'A' && first <= 'Z' {
			first += 'a' - 'A'
		}
		return string(first) + s[1:]
	}
	return s
}

// applySetModifiers applies the modifiers in order of precedence (higher
// first). A :quotewildcard result that would be truncated to maxLen is
// cut so that no dangling backslash remains.
func applySetModifiers(maxLen int, mods []string, s string) string {
	lastPrec := 9999
	for _, prec := range [5]int{40, 30, 20, 15, 10} {
		for _, m := range mods {
			if modifierPrecedence(m) == prec {
				if m == "encodeurl" && maxLen > 0 {
					s = encodeURLLimited(s, maxLen) // whole characters only
				} else {
					s = applyModifier(m, s)
				}
				lastPrec = prec
			}
		}
	}
	if lastPrec == 15 {
		return s
	}
	if lastPrec == 20 && maxLen > 0 && len(s) > maxLen {
		until := maxLen
		for until > 0 && s[until] >= 128 && s[until] < 192 {
			until--
		}
		if until > 0 && s[until-1] == '\\' {
			until--
		}
		s = s[:until]
	}
	return s
}

type TestString struct {
	matcherTest

	Source []string
}

func (t TestString) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if !t.regexKeysValid(d) {
		return false, nil
	}
	entryCount := uint64(0)
	for _, source := range t.Source {
		source = expandVars(d, source)

		if t.isCount() {
			if source != "" {
				entryCount++
			}
			continue
		}

		ok, err := t.matcherTest.tryMatch(d, source)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}

	if t.isCount() {
		return t.countMatches(d, entryCount), nil
	}

	return false, nil
}

func init() {
	gob.Register(TestString{})
	gob.Register(CmdSet{})
}

// encodeURLLimited percent-encodes s but stops before a character whose
// encoding would exceed maxLen bytes, so the value never ends inside a
// multi-octet character's escape sequence.
func encodeURLLimited(s string, maxLen int) string {
	var b strings.Builder
	for _, r := range s {
		enc := EncodeURL(string(r))
		if b.Len()+len(enc) > maxLen {
			break
		}
		b.WriteString(enc)
	}
	return b.String()
}
