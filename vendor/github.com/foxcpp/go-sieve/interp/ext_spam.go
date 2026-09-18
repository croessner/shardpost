package interp

import (
	"context"
	"encoding/gob"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/foxcpp/go-sieve/parser"
)

// spamtest, spamtestplus and virustest (RFC 5235), shardpost fork. The
// scores come from message headers named in the configuration (Pigeonhole
// key names):
//
//	sieve_spamtest_status_header    "X-Spam-Score" or "X-Spam-Status:regex with one group"
//	sieve_spamtest_status_type      score | strlen | text | yesno
//	sieve_spamtest_score_max_header "X-Spam-Status:regex with one group"
//	sieve_spamtest_score_max_value  number
//	sieve_spamtest_text_value/1..10 text for that value
//	sieve_virustest_*               likewise, values 1..5
//
// Without a status header configured the tests yield "0".

type SpamTest struct {
	matcherTest
	Virus   bool
	Percent bool
}

func loadSpamTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("spamtest") && !s.RequiresExtension("spamtestplus") {
		return nil, parser.ErrorAt(test.Position, "missing require 'spamtest'")
	}
	loaded := SpamTest{matcherTest: newMatcherTest()}
	var key []string
	spec := loaded.addSpecTags(&Spec{
		Pos: []SpecPosArg{{MinStrCount: 1, MatchStr: func(v []string) { key = v }}},
	})
	if s.RequiresExtension("spamtestplus") {
		spec.Tags["percent"] = SpecTag{MatchBool: func() { loaded.Percent = true }}
	}
	if err := LoadSpec(s, spec, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, nil
}

func loadVirusTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("virustest") {
		return nil, parser.ErrorAt(test.Position, "missing require 'virustest'")
	}
	loaded := SpamTest{matcherTest: newMatcherTest(), Virus: true}
	var key []string
	spec := loaded.addSpecTags(&Spec{
		Pos: []SpecPosArg{{MinStrCount: 1, MatchStr: func(v []string) { key = v }}},
	})
	if err := LoadSpec(s, spec, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, nil
}

func (t SpamTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	value := t.score(d)
	if t.isCount() {
		return t.countMatches(d, 1), nil
	}
	return t.tryMatch(d, value)
}

// headerValue reads a "Header" or "Header:regex" spec from the message:
// the first header instance, optionally reduced to the regex's first
// group.
func headerValue(d *RuntimeData, spec string) (string, bool) {
	name, re, hasRe := strings.Cut(spec, ":")
	vals, err := d.Msg.HeaderGet(strings.TrimSpace(name))
	if err != nil || len(vals) == 0 {
		return "", false
	}
	v := strings.TrimSpace(vals[0])
	if !hasRe {
		return v, true
	}
	rx, err := regexp.Compile(strings.TrimSpace(re))
	if err != nil {
		return "", false
	}
	m := rx.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	if len(m) > 1 {
		return m[1], true
	}
	return m[0], true
}

func (t SpamTest) score(d *RuntimeData) string {
	prefix := "sieve_spamtest_"
	scale := 10.0
	if t.Virus {
		prefix = "sieve_virustest_"
		scale = 5
	}
	conf := d.Script.opts.Conf
	hdr := conf(prefix + "status_header")
	if hdr == "" {
		return "0"
	}
	raw, ok := headerValue(d, hdr)
	if !ok {
		return "0"
	}
	typ := conf(prefix + "status_type")
	var level float64 // 0 = unknown, otherwise 1..scale
	var percent float64
	havePercent := false
	switch typ {
	case "text":
		for i := 1; i <= int(scale); i++ {
			if v := conf(prefix + "text_value/" + strconv.Itoa(i)); v != "" && strings.EqualFold(strings.TrimSpace(v), raw) {
				level = float64(i)
			}
		}
	case "yesno":
		switch strings.ToLower(raw) {
		case "yes":
			level = scale
		case "no":
			level = 1
		}
	default: // score, strlen
		var f float64
		if typ == "strlen" {
			f = float64(len(raw))
		} else {
			var err error
			f, err = strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				return "0"
			}
		}
		max := 0.0
		if m := conf(prefix + "score_max_header"); m != "" {
			if mv, ok := headerValue(d, m); ok {
				max, _ = strconv.ParseFloat(strings.TrimSpace(mv), 64)
			}
		}
		if max == 0 {
			max, _ = strconv.ParseFloat(conf(prefix+"score_max_value"), 64)
		}
		if max <= 0 {
			return "0"
		}
		if f < 0 {
			f = 0
		}
		if f > max {
			f = max
		}
		level = 1 + math.Floor(f/max*(scale-1)+1e-9)
		percent = math.Floor(f/max*100 + 1e-9)
		havePercent = true
	}
	if level == 0 {
		return "0"
	}
	if t.Percent {
		if !havePercent {
			percent = math.Floor((level - 1) / (scale - 1) * 100)
		}
		return strconv.Itoa(int(percent))
	}
	return strconv.Itoa(int(level))
}

func init() {
	gob.Register(SpamTest{})
}
