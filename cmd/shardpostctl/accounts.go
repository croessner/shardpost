package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"shardpost/internal/adminapi"
)

// account list [-limit n] [-after cursor] [-state active|purging] [-all]
//
// lists the account directory (role diagnose, scope of all accounts): a
// page in the directory's order, or with -all every page. With --json a
// page prints as one document; -all --json prints one entry per line,
// so the listing of a large directory is never held in memory.
func accountList(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	p := adminapi.ListAccountsParams{Limit: &a.limit, Cursor: optional(a.after), State: optional(a.state)}
	out := &accountPrinter{out: stdout, json: a.asJSON}

	for {
		resp, err := c.ListAccountsWithResponse(ctx, &p)
		if err != nil {
			return transportError(stderr, err)
		}

		page := resp.JSON200
		if page == nil {
			return failure(stderr, resp.JSONDefault, resp.StatusCode())
		}

		if a.asJSON && !a.all {
			return printJSON(stdout, page)
		}

		out.page(page.Items)

		if !a.all || page.Next == nil {
			out.end(stdout, page.Next)
			return exOK
		}

		p.Cursor = page.Next
	}
}

// accountPrinter prints the pages of an account listing as they come: a
// table (aligned per page), or one JSON document per entry.
type accountPrinter struct {
	out    io.Writer
	json   bool
	header bool
}

func (p *accountPrinter) page(items []adminapi.AccountListEntry) {
	if p.json {
		enc := json.NewEncoder(p.out)
		for i := range items {
			_ = enc.Encode(items[i])
		}

		return
	}

	tw := tabwriter.NewWriter(p.out, 0, 4, 2, ' ', 0)

	if !p.header {
		_, _ = fmt.Fprintln(tw, "ACCOUNT\tSTATE\tCREATED\tLAST SCRUB")

		p.header = true
	}

	printAccounts(tw, items)

	_ = tw.Flush()
}

// end names the cursor of the next page.
func (p *accountPrinter) end(stdout io.Writer, next *string) {
	if next != nil && !p.json {
		sayf(stdout, "more: -after %q (or -all)\n", *next)
	}
}

// printAccounts writes the rows of one page.
func printAccounts(w io.Writer, items []adminapi.AccountListEntry) {
	for _, it := range items {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", it.Account, entryState(&it.Entry), it.Entry.Created.Format(time.RFC3339), scrubState(it.Entry.Scrub))
	}
}

// scrubState is the last batch scrub of an entry in a few words.
func scrubState(s *adminapi.AccountScrub) string {
	if s == nil {
		return "-"
	}

	state := "incomplete"
	if s.Complete {
		state = "complete"
	}

	return s.At.Format(time.RFC3339) + " " + state + ", findings " + strconv.Itoa(s.Findings)
}
