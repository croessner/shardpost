package bench

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Counting RADOS operations. The client library logs every operation it
// submits at debug_objecter 10, with the object name, the pool and
// namespace, and the list of its steps:
//
//	... client.4122.objecter _op_submit oid mbox/0a1b '@1;bench-000001' '@1;bench-000001' [omap-set-vals in=14b,assert-version v12] tid 7 osd.0
//
// One such line is one round trip to an OSD, so counting them by object
// class counts what a delivery costs, exactly and without touching the
// store's code. The format is the client library's (the image's
// librados), not the cluster's; TestObjecterLogFormat in cephbench
// checks it against the library the bench runs with.

// ObjecterOptions are the librados options that write the log to path.
func ObjecterOptions(path string) map[string]string {
	const yes, no = "true", "false"

	return map[string]string{
		"debug_objecter": "10/10",
		"log_file":       path,
		"log_to_file":    yes,
		"log_to_stderr":  no,
		"err_to_stderr":  no,
	}
}

var opSubmit = regexp.MustCompile(`_op_submit oid (\S*) '@(-?\d+)(?:;([^']*))?' '[^']*' \[(.*)\] tid \d+`)

// writeSteps are the steps that change an object.
var writeSteps = map[string]bool{
	"write": true, "writefull": true, "writesame": true, "append": true, "truncate": true, "zero": true,
	"delete": true, "create": true, "omap-set-vals": true, "omap-set-header": true, "omap-rm-keys": true,
	"omap-rm-key-range": true, "omap-clear": true, "setxattr": true, "rmxattr": true, "rollback": true,
}

// OpCount counts operations (round trips): Writes changed the object
// (at least one write step), Calls ran an object class method and
// wrote nothing else, Reads are the rest.
type OpCount struct {
	Ops    int64 `json:"ops"`
	Reads  int64 `json:"reads"`
	Writes int64 `json:"writes"`
	Calls  int64 `json:"calls"`
}

func (c *OpCount) add(o OpCount) {
	c.Ops += o.Ops
	c.Reads += o.Reads
	c.Writes += o.Writes
	c.Calls += o.Calls
}

// Per returns the counts divided by n.
func (c OpCount) Per(n int64) OpRate {
	if n <= 0 {
		return OpRate{}
	}

	d := float64(n)

	return OpRate{Ops: round3(float64(c.Ops) / d), Reads: round3(float64(c.Reads) / d),
		Writes: round3(float64(c.Writes) / d), Calls: round3(float64(c.Calls) / d)}
}

// OpRate is an OpCount per unit (delivery, account).
type OpRate struct {
	Ops    float64 `json:"ops"`
	Reads  float64 `json:"reads"`
	Writes float64 `json:"writes"`
	Calls  float64 `json:"calls"`
}

// OpCounts are the counted operations by pool and object class.
type OpCounts map[string]map[string]*OpCount

func (o OpCounts) add(pool, class string, c OpCount) {
	if o[pool] == nil {
		o[pool] = map[string]*OpCount{}
	}

	if o[pool][class] == nil {
		o[pool][class] = &OpCount{}
	}

	o[pool][class].add(c)
}

// Total sums all pools and classes.
func (o OpCounts) Total() OpCount {
	var t OpCount

	for _, classes := range o {
		for _, c := range classes {
			t.add(*c)
		}
	}

	return t
}

// ParseObjecterLog counts the operations of an objecter log by pool and
// class. pools maps pool ids to names (other pools are skipped); keep
// selects namespaces (nil: all).
func ParseObjecterLog(r io.Reader, pools map[int64]string, keep func(ns string) bool) (OpCounts, error) {
	counts := OpCounts{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)

	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "_op_submit oid ") {
			continue
		}

		m := opSubmit.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("objecter log: unexpected line %q", line)
		}

		id, _ := strconv.ParseInt(m[2], 10, 64)

		pool, ok := pools[id]
		if !ok || (keep != nil && !keep(m[3])) {
			continue
		}

		counts.add(pool, Classify(m[1]), classifySteps(m[4]))
	}

	return counts, sc.Err()
}

// classifySteps counts one operation from its step list.
func classifySteps(steps string) OpCount {
	call := false

	for step := range strings.SplitSeq(steps, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(step), " ")
		if writeSteps[name] {
			return OpCount{Ops: 1, Writes: 1}
		}

		call = call || name == "call"
	}

	if call {
		return OpCount{Ops: 1, Calls: 1}
	}

	return OpCount{Ops: 1, Reads: 1}
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }
