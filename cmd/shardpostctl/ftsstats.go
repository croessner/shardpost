package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"shardpost/internal/adminapi"
)

// fts stats [-exact] [-parallel n] [-classes] [-json] <account>   role diagnose
//
// Key counts of an account's full-text index (temp/13 §7.1). Without
// options the counters of the posting objects; -exact counts every
// object's keys; -classes classifies the terms, only for the test
// accounts of fts.stats_test_accounts (otherwise refused, exit 77).
// Nothing printed is a term.

const ftsStatsUsage = "usage: shardpostctl fts stats [-exact] [-parallel n] [-classes] [-json] <account>"

func ftsStatsCmd(client *adminapi.ClientWithResponses, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmdFTS+" stats", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		asJSON, exact, classes bool
		parallel               int
	)

	fs.BoolVar(&asJSON, "json", false, "print JSON")
	fs.BoolVar(&exact, "exact", false, "count the keys of every object page by page (thousands of reads on a large account)")
	fs.IntVar(&parallel, "parallel", 2, "-exact: objects counted at the same time (1 to 16)")
	fs.BoolVar(&classes, "classes", false, "classify the terms (test accounts of fts.stats_test_accounts only)")

	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return exUsage
	}

	if len(rest) != 1 || parallel < 1 || parallel > 16 {
		sayln(stderr, ftsStatsUsage)
		return exUsage
	}

	p := adminapi.GetFTSStatsParams{}
	if exact {
		p.Exact, p.Parallel = &exact, &parallel
	}

	if classes {
		p.Classes = &classes
	}

	return request(stderr, func() (*adminapi.FTSStats, *adminapi.Error, int, error) {
		resp, err := client.GetFTSStatsWithResponse(context.Background(), rest[0], &p)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(st *adminapi.FTSStats) int {
		if asJSON {
			return printJSON(stdout, st)
		}

		printFTSStats(stdout, st)

		return exOK
	})
}

// perMessage is n per document, "" without documents.
func perMessage(n, docs int64) string {
	if docs <= 0 {
		return ""
	}

	return fmt.Sprintf(" (%.1f per message)", float64(n)/float64(docs))
}

// printFTSStats prints the counts: the sums, the objects and, for a test
// account, the classes and the projection per identifier bound.
func printFTSStats(w io.Writer, st *adminapi.FTSStats) {
	mode := "counters"
	if st.Exact {
		mode = "counted keys"
	}

	rows := [][2]string{{"shape", shapeText(st.Shape)}, {"source", mode}, {"docs", fmt.Sprint(st.Docs)},
		{"terms", fmt.Sprint(st.Terms) + perMessage(st.Terms, st.Docs)},
		{"dict_keys", fmt.Sprint(st.DictKeys) + perMessage(st.DictKeys, st.Docs)}}

	if st.Shape.D != nil && *st.Shape.D > 0 {
		rows = append(rows, [2]string{"dict_keys per object", fmt.Sprintf("%d (mean of %d)", st.DictKeys/int64(*st.Shape.D), *st.Shape.D)})
	}

	if st.Exact {
		rows = append(rows, [2]string{"header_keys", fmt.Sprint(st.HeaderKeys)})
	}

	table(w, rows)

	for _, o := range st.Objects {
		line := fmt.Sprintf("object %s  %s  keys %d", o.Object, o.Kind, o.Keys)
		if o.DictKeys != nil {
			line += fmt.Sprintf("  dict_keys %d", *o.DictKeys)
		}

		sayln(w, line)
	}

	c := st.Classes
	if c == nil {
		return
	}

	sayf(w, "classes: %d terms, %d dictionary keys%s\n", c.Terms, c.DictKeys, perMessage(c.DictKeys, st.Docs))
	sayf(w, "%-6s %-8s %-6s %10s %12s\n", "field", "class", "length", "terms", "dict_keys")

	for _, r := range c.Rows {
		sayf(w, "%-6s %-8s %-6s %10d %12d\n", r.Field, r.Class, r.Length, r.Terms, r.DictKeys)
	}

	sayf(w, "identifier bound n: dictionary keys with one key per identifier term (letters and digits) of at least n runes in the message text\n")
	sayf(w, "%-4s %12s %12s %12s\n", "n", "dict_keys", "per_message", "identifiers")

	for _, b := range c.ByIdentMinRunes {
		per := "-"
		if st.Docs > 0 {
			per = fmt.Sprintf("%.1f", float64(b.DictKeys)/float64(st.Docs))
		}

		sayf(w, "%-4d %12d %12s %12d\n", b.MinRunes, b.DictKeys, per, b.Identifiers)
	}
}
