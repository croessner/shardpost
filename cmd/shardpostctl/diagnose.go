package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// Read-only diagnosis (role diagnose):
//
//	server status
//	account show <account>
//	mailbox list [-limit n] [-after name] [-all] <account>
//	mailbox show <account> <mailbox name or id>
//	acl list <account> <mailbox>
//	acl explain <account> <mailbox> <principal>
//	quota show <account>
//	shared list <account>
//	account directory <account>
//	account list [-limit n] [-after cursor] [-state active|purging] [-all]
//
// Every command takes --json.

const diagnoseUsage = `usage: shardpostctl server status | account show <account> | mailbox list [-limit n] [-after name] [-all] <account> |
  mailbox show <account> <mailbox> | acl list <account> <mailbox> | acl explain <account> <mailbox> <principal> |
  quota show <account> | shared list <account> | account directory <account> |
  account list [-limit n] [-after cursor] [-state active|purging] [-all]   (each with --json) |
  mailbox create [-special-use \Archive] <account> <name> | mailbox rename [-if-match E] <account> <mailbox> <new> |
  mailbox subscribe|unsubscribe <account> <mailbox> | acl set [-mode add|remove] [-if-match E] <account> <mailbox> <identifier> <rights> |
  acl delete [-if-match E] <account> <mailbox> <identifier> | account repair <account>`

// labelStore labels the store row.
const labelStore = "store"

// diagArgs are the parsed arguments of a diagnosis command.
type diagArgs struct {
	args   []string
	asJSON bool
	limit  int
	after  string
	all    bool
	state  string
	// provisioning flags
	specialUse string
	mode       string
	ifMatch    string
}

// diagCmd dispatches "<noun> <verb> ...".
func diagCmd(client *adminapi.ClientWithResponses, noun string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		sayln(stderr, diagnoseUsage)
		return exUsage
	}

	verb := noun + " " + args[0]

	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var a diagArgs

	fs.BoolVar(&a.asJSON, "json", false, "print JSON")
	fs.IntVar(&a.limit, "limit", 100, "mailboxes or accounts per page (mailbox list, account list)")
	fs.StringVar(&a.after, "after", "", "continue after this mailbox name or at this cursor (mailbox list, account list)")
	fs.BoolVar(&a.all, "all", false, "fetch all pages (mailbox list, account list)")
	fs.StringVar(&a.state, labelState, "", "account list: only entries in this state (active, purging)")
	fs.StringVar(&a.specialUse, "special-use", "", `special-use of a new mailbox, comma-separated (e.g. \Archive)`)
	fs.StringVar(&a.mode, "mode", "replace", "acl set: replace, add or remove")
	fs.StringVar(&a.ifMatch, "if-match", "", "ETag the change is based on (412 if it changed)")

	var err error
	if a.args, err = parseInterleaved(fs, args[1:]); err != nil {
		return exUsage
	}

	run, n := command(verb)
	if run == nil || len(a.args) != n {
		sayln(stderr, diagnoseUsage)
		return exUsage
	}

	return run(context.Background(), client, a, stdout, stderr)
}

// parseInterleaved parses flags before, between and after the
// positional arguments and returns the positional ones.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string

	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}

		if fs.NArg() == 0 {
			return positional, nil
		}

		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type diagFunc func(context.Context, *adminapi.ClientWithResponses, diagArgs, io.Writer, io.Writer) int

// command returns a diagnosis or provisioning command and its number of
// positional arguments.
func command(verb string) (diagFunc, int) {
	if run, n := provisionCommand(verb); run != nil {
		return run, n
	}

	return diagCommand(verb)
}

// diagCommand returns the command and its number of positional arguments.
func diagCommand(verb string) (diagFunc, int) {
	switch verb {
	case cmdServer + " status":
		return serverStatus, 0
	case "account show":
		return accountShow, 1
	case "mailbox list":
		return mailboxList, 1
	case "mailbox show":
		return mailboxShow, 2
	case "acl list":
		return aclList, 2
	case "acl explain":
		return aclExplain, 3
	case "quota show":
		return quotaShow, 1
	case "account directory":
		return accountDirectory, 1
	case "account list":
		return accountList, 0
	case "shared list":
		return sharedList, 1
	default:
		return nil, 0
	}
}

// transportError reports a failed request (no answer from the server).
func transportError(stderr io.Writer, err error) int {
	sayln(stderr, "shardpostctl:", err)
	return exTempFail
}

func printJSON(stdout io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)

	return exOK
}

// table writes aligned "key value" lines.
func table(stdout io.Writer, rows [][2]string) {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1])
	}

	_ = tw.Flush()
}

func serverStatus(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	resp, err := c.GetServerStatusWithResponse(ctx)
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	st := resp.JSON200
	if a.asJSON {
		return printJSON(stdout, st)
	}

	store := stateOK
	if !st.Store.Ok {
		store = "unavailable: " + deref(st.Store.Error)
	}

	table(stdout, [][2]string{{cmdVersion, st.Version + " (" + st.GoVersion + ")"}, {"instance", st.Instance},
		{"started", st.StartedAt.Format(time.RFC3339)}, {"config", st.ConfigFingerprint}, {labelStore, store}})

	return exOK
}

func accountShow(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	resp, err := c.GetAccountWithResponse(ctx, a.args[0])
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	st := resp.JSON200
	if a.asJSON {
		return printJSON(stdout, st)
	}

	trace := traceOffText
	if st.TraceActive && st.TraceUntil != nil {
		trace = "on until " + st.TraceUntil.Format(time.RFC3339)
	}

	table(stdout, [][2]string{{cmdAccount, st.Account}, {"lookup", string(st.Lookup) + " " + deref(st.CanonicalAccount)},
		{labelStore, fmt.Sprintf("%v, %d mailboxes", st.StoreExists, st.Mailboxes)}, {cmdQuota, quotaText(st.Quota)},
		{"import lock", fmt.Sprint(st.ImportLocked)}, {cmdTrace, trace}})

	return exOK
}

// quotaText renders usage/limit (0 = unlimited).
func quotaText(q adminapi.QuotaStatus) string {
	limit := func(n int64) string {
		if n == 0 {
			return "unlimited"
		}

		return fmt.Sprint(n)
	}

	return fmt.Sprintf("%d / %s bytes, %d / %s messages", q.UsageBytes, limit(q.LimitBytes), q.UsageMessages, limit(q.LimitMessages))
}

func quotaShow(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	resp, err := c.GetQuotaWithResponse(ctx, a.args[0])
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	if a.asJSON {
		return printJSON(stdout, resp.JSON200)
	}

	sayln(stdout, quotaText(*resp.JSON200))

	return exOK
}

func mailboxList(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	var items []adminapi.MailboxSummary

	p := adminapi.ListMailboxesParams{Limit: &a.limit, After: optional(a.after)}

	for {
		resp, err := c.ListMailboxesWithResponse(ctx, a.args[0], &p)
		if err != nil {
			return transportError(stderr, err)
		}

		if resp.JSON200 == nil {
			return failure(stderr, resp.JSONDefault, resp.StatusCode())
		}

		items = append(items, resp.JSON200.Items...)

		if !a.all || resp.JSON200.Next == nil {
			if a.asJSON {
				return printJSON(stdout, adminapi.MailboxPage{Items: items, Next: resp.JSON200.Next})
			}

			printMailboxes(stdout, items, resp.JSON200.Next)

			return exOK
		}

		p.After = resp.JSON200.Next
	}
}

func printMailboxes(stdout io.Writer, items []adminapi.MailboxSummary, next *string) {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tID\tUIDVALIDITY\tUIDNEXT\tMODSEQ\tSUB\tSPECIAL\tACL")

	for _, m := range items {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%v\t%s\t%d\n", m.Name, m.Id, m.Uidvalidity, m.UidNext, m.HighestModseq,
			m.Subscribed, strings.Join(m.SpecialUse, ","), m.AclEntries)
	}

	_ = tw.Flush()

	if next != nil {
		sayf(stdout, "more: -after %q (or -all)\n", *next)
	}
}

// resolveMailbox finds a mailbox by name, else treats the argument as id.
func resolveMailbox(ctx context.Context, c *adminapi.ClientWithResponses, account, mailbox string, stderr io.Writer) (string, int, bool) {
	resp, err := c.ListMailboxesWithResponse(ctx, account, &adminapi.ListMailboxesParams{Name: &mailbox})
	if err != nil {
		return "", transportError(stderr, err), false
	}

	if resp.JSON200 == nil {
		return "", failure(stderr, resp.JSONDefault, resp.StatusCode()), false
	}

	if len(resp.JSON200.Items) == 1 {
		return resp.JSON200.Items[0].Id, exOK, true
	}

	return mailbox, exOK, true
}

func mailboxShow(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	id, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	resp, err := c.GetMailboxWithResponse(ctx, a.args[0], id)
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	d := resp.JSON200
	if a.asJSON {
		return printJSON(stdout, d)
	}

	m := d.Mailbox
	table(stdout, [][2]string{{labelName, m.Name}, {labelID, m.Id}, {labelValidity, fmt.Sprint(m.Uidvalidity)},
		{"uidnext", fmt.Sprint(m.UidNext)}, {"highestmodseq", fmt.Sprint(m.HighestModseq)}, {"log start", fmt.Sprint(d.LogStart)},
		{"messages", fmt.Sprintf("%d (%d unseen, %d deleted)", d.Messages, d.Unseen, d.Deleted)}, {labelSize, fmt.Sprintf("%d bytes", d.SizeBytes)},
		{"shards", fmt.Sprintf("%d of %d UIDs", d.Shards, m.ShardSize)}, {"subscribed", fmt.Sprint(m.Subscribed)},
		{"special use", strings.Join(m.SpecialUse, ",")}, {"keywords", strings.Join(d.Keywords, " ")}, {"acl entries", fmt.Sprint(m.AclEntries)}})

	return exOK
}

func aclList(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	id, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	resp, err := c.GetMailboxACLWithResponse(ctx, a.args[0], id)
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printACL(stdout, a, resp.JSON200, resp.HTTPResponse.Header.Get("ETag"))
}

// printACL prints an ACL with its ETag.
func printACL(stdout io.Writer, a diagArgs, acl *adminapi.MailboxACL, etag string) int {
	if a.asJSON {
		return printJSON(stdout, acl)
	}

	var rows [][2]string

	// A public folder has no owner: only the entries grant rights.
	if !model.IsPublic(acl.Owner) {
		rows = append(rows, [2]string{acl.Owner, model.AllRights + " (owner)"})
	}

	for _, e := range acl.Entries {
		rows = append(rows, [2]string{e.Identifier, e.Rights})
	}

	table(stdout, rows)
	sayf(stdout, "generation %d, etag %s\n", acl.Generation, etag)

	return exOK
}

func aclExplain(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	id, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	resp, err := c.ExplainMailboxACLWithResponse(ctx, a.args[0], id, &adminapi.ExplainMailboxACLParams{Principal: a.args[2]})
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	e := resp.JSON200
	if a.asJSON {
		return printJSON(stdout, e)
	}

	table(stdout, [][2]string{{labelPrincipal, e.Principal + " (" + string(e.Lookup) + " " + deref(e.CanonicalPrincipal) + ")"},
		{"rights", e.Rights}, {"own entry", e.OwnEntry}, {"anyone", e.AnyoneEntry}, {"visible", fmt.Sprint(e.Visible)},
		{"pointer", fmt.Sprint(e.Pointer.Present)}})

	for _, f := range e.Findings {
		sayln(stdout, "- "+f)
	}

	return exOK
}

func sharedList(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	resp, err := c.ListSharedMailboxesWithResponse(ctx, a.args[0])
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	if a.asJSON {
		return printJSON(stdout, resp.JSON200)
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "OWNER\tMAILBOX\tSUBSCRIBED\tGENERATION")

	for _, r := range resp.JSON200.Items {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%v\t%d\n", r.Owner, r.Mailbox, r.Subscribed, r.Generation)
	}

	_ = tw.Flush()

	return exOK
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
