package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"shardpost/internal/adminapi"
)

// Layouts of the full-text index (stage B14b):
//
//	fts layout3 status [-json]                         role diagnose
//	fts layout3 enable -pods n [-force] [-json]        role repair
//	fts layout3 disable [-json]                        role repair (exit 65 while accounts have layout 3)
//	fts layout3 list [-verify] [-after a] [-limit n] [-json]  role diagnose, scope of all accounts
//	fts dictfilter status [-json]                      role diagnose (stage B14h)
//	fts dictfilter enable -pods n [-force] [-json]     role repair
//	fts dictfilter disable [-json]                     role repair, scope of all accounts (exit 65 while generations have it)
//	fts layout show [-json] <account>                  role diagnose
//	fts layout clear [-json] <account>                 role repair
//	fts stats [-exact] [-parallel n] [-classes] [-json] <account>  role diagnose (ftsstats.go)

// Verbs and labels shared with other commands.
const (
	verbShow       = "show"
	verbStatus     = "status"
	verbEnable     = "enable"
	verbDisable    = "disable"
	labelPrincipal = "principal"
	labelNext      = "next"
)

const (
	cmdFTS   = "fts"
	ftsUsage = "usage: shardpostctl fts layout3 status|enable -pods n [-force]|disable|list [-verify] [-after a] [-limit n] [-json] | " +
		"fts dictfilter status|enable -pods n [-force]|disable [-json] | " +
		"fts layout show|clear [-json] <account> | fts stats [-exact] [-parallel n] [-classes] [-json] <account>"
)

// ftsArgs are the flags of the fts commands.
type ftsArgs struct {
	asJSON, force, verify bool
	pods, limit           int
	after                 string
	rest                  []string
}

// ftsCmd dispatches the fts commands.
func ftsCmd(client *adminapi.ClientWithResponses, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "stats" {
		return ftsStatsCmd(client, args[1:], stdout, stderr)
	}

	if len(args) < 2 {
		sayln(stderr, ftsUsage)
		return exUsage
	}

	group, verb := args[0], args[1]

	fs := flag.NewFlagSet(cmdFTS+" "+group+" "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var a ftsArgs

	fs.BoolVar(&a.asJSON, "json", false, "print JSON")
	fs.BoolVar(&a.force, "force", false, "enable: set the mark without the registrations check (audited)")
	fs.BoolVar(&a.verify, "verify", false, "list: read each account's layout record")
	fs.IntVar(&a.pods, "pods", 0, "enable: the number of servers running now (required)")
	fs.IntVar(&a.limit, "limit", 100, "list: accounts per page")
	fs.StringVar(&a.after, "after", "", "list: continue after this account")

	rest, err := parseInterleaved(fs, args[2:])
	if err != nil {
		return exUsage
	}

	a.rest = rest

	return ftsGroupCmd(context.Background(), client, group, verb, a, stdout, stderr)
}

// ftsGroupCmd runs a command of a group of fts.
func ftsGroupCmd(ctx context.Context, client *adminapi.ClientWithResponses, group, verb string, a ftsArgs, stdout, stderr io.Writer) int {
	switch {
	case group == "layout3" && len(a.rest) == 0:
		return layout3Cmd(ctx, client, verb, a, stdout, stderr)
	case group == "dictfilter" && len(a.rest) == 0:
		return dictFilterCmd(ctx, client, verb, a, stdout, stderr)
	case group == "layout" && len(a.rest) == 1 && (verb == verbShow || verb == "clear"):
		return accountLayoutCmd(ctx, client, verb, a.rest[0], a, stdout, stderr)
	default:
		sayln(stderr, ftsUsage)
		return exUsage
	}
}

// dictFilterCmd runs the commands of the cluster mark of the dictionary
// filter (stage B14h).
func dictFilterCmd(ctx context.Context, c *adminapi.ClientWithResponses, verb string, a ftsArgs, stdout, stderr io.Writer) int {
	show := func(st *adminapi.FTSLayout3Status) int { return printLayout3(stdout, st, a.asJSON) }

	switch verb {
	case verbStatus:
		return request(stderr, func() (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
			resp, err := c.GetFTSDictFilterWithResponse(ctx)
			if err != nil {
				return nil, nil, 0, err
			}

			return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
		}, show)
	case verbEnable:
		if a.pods < 1 {
			sayln(stderr, "shardpostctl: fts dictfilter enable needs -pods <number of running servers>")
			return exUsage
		}

		body := adminapi.FTSLayout3Enable{Pods: a.pods}
		if a.force {
			body.Force = &a.force
		}

		return request(stderr, func() (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
			resp, err := c.EnableFTSDictFilterWithResponse(ctx, body)
			if err != nil {
				return nil, nil, 0, err
			}

			return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
		}, show)
	case verbDisable:
		return request(stderr, func() (*adminapi.FTSLayout3Disable, *adminapi.Error, int, error) {
			resp, err := c.DisableFTSDictFilterWithResponse(ctx)
			if err != nil {
				return nil, nil, 0, err
			}

			return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
		}, func(d *adminapi.FTSLayout3Disable) int {
			return printDisable(stdout, stderr, d, a.asJSON, "the dictionary filter mark removed; no generation has the filter or the text preparation nosig",
				"have a generation with the dictionary filter or the text preparation nosig: downgrade not possible yet "+
					"(convert them with fts_layout -dict-filter none, rebuild those with nosig with fts_rebuild)")
		})
	default:
		sayln(stderr, ftsUsage)
		return exUsage
	}
}

// layout3Cmd runs the cluster commands.
func layout3Cmd(ctx context.Context, c *adminapi.ClientWithResponses, verb string, a ftsArgs, stdout, stderr io.Writer) int {
	switch verb {
	case verbStatus:
		return request(stderr, func() (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
			resp, err := c.GetFTSLayout3WithResponse(ctx)
			if err != nil {
				return nil, nil, 0, err
			}

			return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
		}, func(st *adminapi.FTSLayout3Status) int { return printLayout3(stdout, st, a.asJSON) })
	case verbEnable:
		return layout3Enable(ctx, c, a, stdout, stderr)
	case verbDisable:
		return layout3Disable(ctx, c, a, stdout, stderr)
	case verbList:
		return layout3List(ctx, c, a, stdout, stderr)
	default:
		sayln(stderr, ftsUsage)
		return exUsage
	}
}

func layout3Enable(ctx context.Context, c *adminapi.ClientWithResponses, a ftsArgs, stdout, stderr io.Writer) int {
	// Servers before 0.45.0 do not register: without the number of pods
	// "every registered server" would be trivially true (N2).
	if a.pods < 1 {
		sayln(stderr, "shardpostctl: fts layout3 enable needs -pods <number of running servers>")
		return exUsage
	}

	body := adminapi.FTSLayout3Enable{Pods: a.pods}
	if a.force {
		body.Force = &a.force
	}

	return request(stderr, func() (*adminapi.FTSLayout3Status, *adminapi.Error, int, error) {
		resp, err := c.EnableFTSLayout3WithResponse(ctx, body)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(st *adminapi.FTSLayout3Status) int { return printLayout3(stdout, st, a.asJSON) })
}

func layout3Disable(ctx context.Context, c *adminapi.ClientWithResponses, a ftsArgs, stdout, stderr io.Writer) int {
	return request(stderr, func() (*adminapi.FTSLayout3Disable, *adminapi.Error, int, error) {
		resp, err := c.DisableFTSLayout3WithResponse(ctx)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(d *adminapi.FTSLayout3Disable) int {
		return printDisable(stdout, stderr, d, a.asJSON, "layout 3 mark removed; no account has layout 3",
			"still have layout 3: downgrade not possible yet (convert them with fts_layout layout 2)")
	})
}

// printDisable prints what a disable of a cluster mark left: the
// accounts that still need the mark's version (exit 65), or done.
func printDisable(stdout, stderr io.Writer, d *adminapi.FTSLayout3Disable, asJSON bool, done, still string) int {
	if asJSON {
		printJSON(stdout, d)
	} else {
		for _, acct := range d.Accounts {
			sayln(stdout, acct)
		}
	}

	if len(d.Accounts) == 0 {
		if !asJSON {
			sayln(stdout, done)
		}

		return exOK
	}

	more := ""
	if d.More != nil && *d.More {
		more = " (and more)"
	}

	sayf(stderr, "shardpostctl: the mark is removed, but %d accounts%s %s\n", len(d.Accounts), more, still)

	return exDataErr
}

func layout3List(ctx context.Context, c *adminapi.ClientWithResponses, a ftsArgs, stdout, stderr io.Writer) int {
	p := adminapi.ListFTSLayout3AccountsParams{After: optional(a.after), Limit: &a.limit}
	if a.verify {
		p.Verify = &a.verify
	}

	return request(stderr, func() (*adminapi.FTSLayoutAccountPage, *adminapi.Error, int, error) {
		resp, err := c.ListFTSLayout3AccountsWithResponse(ctx, &p)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(page *adminapi.FTSLayoutAccountPage) int {
		if a.asJSON {
			return printJSON(stdout, page)
		}

		for _, it := range page.Items {
			sayln(stdout, layoutAccountLine(it))
		}

		if a.verify {
			if account, d := largestDict(page.Items); d > 0 {
				sayf(stdout, "largest D on this page: %d (%s)\n", d, account)
			}
		}

		if page.Next != nil {
			sayf(stdout, "more: -after %s\n", *page.Next)
		}

		return exOK
	})
}

// largestDict is the account with the most dictionary objects among
// verified items and their number (0: none). Before a way back to a
// version before 0.49.0, fts.shards_max must be at least the largest D
// in the store, or that version shrinks the dictionary (temp/13 §5.4).
func largestDict(items []adminapi.FTSLayoutAccount) (string, int) {
	var (
		account string
		top     int
	)

	for _, it := range items {
		if it.Layout == nil {
			continue
		}

		for _, sh := range append([]adminapi.FTSLayoutShape{it.Layout.Current}, derefShapes(it.Layout.Next)...) {
			if sh.D != nil && *sh.D > top {
				account, top = it.Account, *sh.D
			}
		}
	}

	return account, top
}

func derefShapes(s *adminapi.FTSLayoutShape) []adminapi.FTSLayoutShape {
	if s == nil {
		return nil
	}

	return []adminapi.FTSLayoutShape{*s}
}

// layoutAccountLine is one account of the presence index.
func layoutAccountLine(it adminapi.FTSLayoutAccount) string {
	line := it.Account

	if it.Since != nil {
		line += "  since " + it.Since.UTC().Format(time.RFC3339)
	}

	if it.Layout != nil {
		line += "  " + shapeText(it.Layout.Current)
	}

	if it.Mismatch != nil && *it.Mismatch {
		line += "  MISMATCH (no layout 3 index)"
	}

	if it.Error != nil {
		line += "  error: " + *it.Error
	}

	return line
}

// printLayout3 prints the mark and the registrations.
func printLayout3(stdout io.Writer, st *adminapi.FTSLayout3Status, asJSON bool) int {
	if asJSON {
		return printJSON(stdout, st)
	}

	rows := [][2]string{{"enabled", fmt.Sprint(st.Enabled)}}

	if m := st.Mark; m != nil {
		rows = append(rows, [2]string{"enabled_at", m.EnabledAt.UTC().Format(time.RFC3339)}, [2]string{"pods", fmt.Sprint(m.Pods)})

		if m.Principal != nil {
			rows = append(rows, [2]string{labelPrincipal, *m.Principal})
		}

		if m.Forced != nil && *m.Forced {
			rows = append(rows, [2]string{"forced", fmt.Sprint(*m.Forced)})
		}
	}

	table(stdout, rows)

	for _, s := range st.Servers {
		fresh := "stale"
		if s.Fresh {
			fresh = "fresh"
		}

		sayf(stdout, "server %s  %s  %s  seen %s  caps %s\n", s.Instance, s.Version, fresh, s.Seen.UTC().Format(time.RFC3339), strings.Join(s.Caps, ","))
	}

	return exOK
}

// accountLayoutCmd shows or clears an account's layout record.
func accountLayoutCmd(ctx context.Context, c *adminapi.ClientWithResponses, verb, account string, a ftsArgs, stdout, stderr io.Writer) int {
	if verb == verbShow {
		return request(stderr, func() (*adminapi.FTSLayoutState, *adminapi.Error, int, error) {
			resp, err := c.GetFTSLayoutWithResponse(ctx, account)
			if err != nil {
				return nil, nil, 0, err
			}

			return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
		}, func(st *adminapi.FTSLayoutState) int {
			if a.asJSON {
				return printJSON(stdout, st)
			}

			printLayoutState(stdout, st)

			return exOK
		})
	}

	return request(stderr, func() (*adminapi.FTSLayoutClear, *adminapi.Error, int, error) {
		resp, err := c.ClearFTSLayoutWithResponse(ctx, account)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(cl *adminapi.FTSLayoutClear) int {
		if a.asJSON {
			return printJSON(stdout, cl)
		}

		if cl.Cleared == "" {
			sayln(stdout, "nothing cleared (no conversion, or its run still works)")
		} else {
			sayln(stdout, "cleared", cl.Cleared)
		}

		printLayoutState(stdout, &cl.State)

		return exOK
	})
}

// printLayoutState prints a layout record.
func printLayoutState(stdout io.Writer, st *adminapi.FTSLayoutState) {
	rows := [][2]string{{"current", shapeText(st.Current)}}

	if st.Next != nil {
		rows = append(rows, [2]string{labelNext, shapeText(*st.Next)})
	}

	if st.NextTok != nil {
		rows = append(rows, [2]string{"next_tok", *st.NextTok})
	}

	if st.NextAt != nil {
		rows = append(rows, [2]string{"next_at", st.NextAt.UTC().Format(time.RFC3339)})
	}

	if st.Old != nil {
		for i, o := range *st.Old {
			rows = append(rows, [2]string{"old", shapeText(o) + oldRun(st, i)})
		}
	}

	table(stdout, rows)
}

// oldRun is the run that left old entry i, with its heartbeat for an
// import reshard (" tok <token> at <time>").
func oldRun(st *adminapi.FTSLayoutState, i int) string {
	var text string

	if st.OldToks != nil && i < len(*st.OldToks) {
		text = " tok " + (*st.OldToks)[i]
	}

	if st.OldAt != nil && i < len(*st.OldAt) && (*st.OldAt)[i] != "" {
		text += " at " + (*st.OldAt)[i]
	}

	return text
}

// shapeText is a generation as "layout 3 gen 9c1e… S/D/H 1/1/1".
func shapeText(s adminapi.FTSLayoutShape) string {
	text := fmt.Sprintf("layout %d", s.Layout)

	if s.Gen != nil && *s.Gen != "" {
		text += " gen " + *s.Gen
	}

	if s.S != nil && s.D != nil && s.H != nil {
		text += fmt.Sprintf(" S/D/H %d/%d/%d", *s.S, *s.D, *s.H)
	}

	if s.DictIdMinRunes != nil && *s.DictIdMinRunes > 0 {
		text += fmt.Sprintf(" dict_filter ids/%d", *s.DictIdMinRunes)
	}

	if s.AttachedHeaders != nil && *s.AttachedHeaders != "" {
		text += " attached_headers " + *s.AttachedHeaders
	}

	return text
}
