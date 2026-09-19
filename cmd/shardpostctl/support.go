package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"shardpost/internal/adminapi"
)

// Support tools (roadmap 8d):
//
//	messages [-after-uid n] [-limit n] <account> <mailbox>          role diagnose
//	search [-mailbox M] [-from s] [-to s] [-subject s] [-message-id s] [-text s]
//	       [-since d] [-before d] [-flag f,..] [-not-flag f,..] [-limit n] [-after c] <account>  role content
//	message show|export [-uidvalidity v] <account> <mailbox> <uid>  role content (export writes RFC 5322 to stdout)
//	flags [-add f,..] [-remove f,..] [-unchanged-since m] [-uidvalidity v] <account> <mailbox> <uid,..>  role support
//	copy|move [-uidvalidity v] <account> <mailbox> <uid,..> <destination>                              role support
//	sieve list|show <account> [name]                                role diagnose / content
//	sieve put [-activate] [-if-match E] [-create] <account> <name> < script    role support
//	sieve delete [-if-match E] <account> <name> | sieve activate <account> <name> | sieve deactivate <account>
//	sieve check [-name n] [-message mailbox:uidvalidity:uid] [-from a] [-to a] <account> [< script|message]
//	learning list [-account A] [-state S] [-limit n] [-after id] | learning show <id> | learning retry [-confirm-repeat] <id>
//
// Without -uidvalidity the mailbox's current one is used; pass the value
// of an earlier listing to make sure the command acts on the same
// messages. Dates are RFC 3339 or YYYY-MM-DD.

const supportUsage = `usage: shardpostctl messages|search|message|flags|copy|move|sieve|learning ... (see shardpostctl -h and docs/admin-api.md)`

// Command nouns of the support tools.
const (
	cmdMessages = "messages"
	cmdSearch   = "search"
	cmdMessage  = "message"
	cmdFlags    = "flags"
	cmdCopy     = "copy"
	cmdMove     = "move"
	cmdSieve    = "sieve"
	cmdLearning = "learning"
)

// supportArgs are the parsed flags of the support commands.
type supportArgs struct {
	args                     []string
	asJSON, activate, create bool
	confirm                  bool
	after, limit             string
	mailbox, from, to        string
	subject, messageID, text string
	since, before            string
	flags, notFlags          string
	add, remove              string
	unchangedSince, validity string
	ifMatch, name, message   string
	account, state, afterUID string
	scriptFile               string
	stdin                    io.Reader
}

func parseSupportArgs(name string, args []string, stderr io.Writer) (supportArgs, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var a supportArgs

	fs.BoolVar(&a.asJSON, "json", false, "print JSON")
	fs.BoolVar(&a.activate, "activate", false, "sieve put: activate the script")
	fs.BoolVar(&a.create, "create", false, "sieve put: only create, never replace")
	fs.BoolVar(&a.confirm, "confirm-repeat", false, "learning retry: the program may see the message again")

	for _, f := range []struct {
		p    *string
		name string
	}{{&a.after, "after"}, {&a.limit, "limit"}, {&a.mailbox, labelMailbox}, {&a.from, "from"}, {&a.to, "to"},
		{&a.subject, "subject"}, {&a.messageID, "message-id"}, {&a.text, "text"}, {&a.since, "since"}, {&a.before, "before"},
		{&a.flags, "flag"}, {&a.notFlags, "not-flag"}, {&a.add, "add"}, {&a.remove, "remove"},
		{&a.unchangedSince, "unchanged-since"}, {&a.validity, labelValidity}, {&a.ifMatch, "if-match"}, {&a.name, labelName},
		{&a.message, "message"}, {&a.account, "account"}, {&a.state, labelState}, {&a.afterUID, "after-uid"},
		{&a.scriptFile, "file"}} {
		fs.StringVar(f.p, f.name, "", f.name)
	}

	var err error
	if a.args, err = parseInterleaved(fs, args); err != nil {
		return a, false
	}

	return a, true
}

// supportCmd dispatches the support commands.
func supportCmd(client *adminapi.ClientWithResponses, noun string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a, ok := parseSupportArgs(noun, args, stderr)
	if !ok {
		return exUsage
	}

	a.stdin = stdin
	verb := noun

	if (noun == cmdMessage || noun == cmdSieve || noun == cmdLearning) && len(a.args) > 0 {
		verb, a.args = noun+" "+a.args[0], a.args[1:]
	}

	run, lo, hi := supportCommand(verb)
	if run == nil || len(a.args) < lo || len(a.args) > hi {
		sayln(stderr, supportUsage)
		return exUsage
	}

	return run(context.Background(), client, a, stdout, stderr)
}

type supportFunc func(context.Context, *adminapi.ClientWithResponses, supportArgs, io.Writer, io.Writer) int

// supportCommand returns a command and its minimum and maximum arity.
func supportCommand(verb string) (supportFunc, int, int) {
	switch verb {
	case cmdMessages:
		return messagesList, 2, 2
	case cmdSearch:
		return search, 1, 1
	case "message show":
		return messageShow, 3, 3
	case "message export":
		return messageExport, 3, 3
	case cmdFlags:
		return flagsCmd, 3, 3
	case cmdCopy:
		return copyCmd(false), 4, 4
	case cmdMove:
		return copyCmd(true), 4, 4
	}

	return sieveOrLearning(verb)
}

func sieveOrLearning(verb string) (supportFunc, int, int) {
	switch verb {
	case "sieve list":
		return sieveList, 1, 1
	case "sieve show":
		return sieveShow, 2, 2
	case "sieve put":
		return sievePut, 2, 2
	case "sieve delete":
		return sieveDelete, 2, 2
	case "sieve activate":
		return sieveActivate, 2, 2
	case "sieve deactivate":
		return sieveActivate, 1, 1
	case "sieve check":
		return sieveCheck, 1, 1
	case "learning list":
		return learningList, 0, 0
	case "learning show":
		return learningShow, 1, 1
	case "learning retry":
		return learningRetry, 1, 1
	default:
		return nil, 0, 0
	}
}

// mailboxState resolves a mailbox (name or id) and its UIDVALIDITY
// (-uidvalidity, else the current one).
func mailboxState(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, account, mailbox string, stderr io.Writer) (string, int64, int, bool) {
	id, code, ok := resolveMailbox(ctx, c, account, mailbox, stderr)
	if !ok {
		return "", 0, code, false
	}

	if a.validity != "" {
		v, err := strconv.ParseInt(a.validity, 10, 64)
		if err != nil {
			sayln(stderr, "shardpostctl: invalid -uidvalidity")
			return "", 0, exUsage, false
		}

		return id, v, exOK, true
	}

	resp, err := c.GetMailboxWithResponse(ctx, account, id)
	if err != nil {
		return "", 0, transportError(stderr, err), false
	}

	if resp.JSON200 == nil {
		return "", 0, failure(stderr, resp.JSONDefault, resp.StatusCode()), false
	}

	return id, resp.JSON200.Mailbox.Uidvalidity, exOK, true
}

func messagesList(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	id, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	p := adminapi.ListMessagesParams{AfterUid: optInt64(a.afterUID), Limit: optInt(a.limit)}

	return request(stderr, func() (*adminapi.MessagePage, *adminapi.Error, int, error) {
		resp, err := c.ListMessagesWithResponse(ctx, a.args[0], id, &p)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(page *adminapi.MessagePage) int {
		if a.asJSON {
			return printJSON(stdout, page)
		}

		sayf(stdout, "uidvalidity %d\n", page.Uidvalidity)

		for _, m := range page.Items {
			printMeta(stdout, m)
		}

		if page.NextUid != nil {
			sayf(stdout, "more: -after-uid %d\n", *page.NextUid)
		}

		return exOK
	})
}

func printMeta(stdout io.Writer, m adminapi.MessageMeta) {
	sayf(stdout, "%8d  %8d  %s  modseq %d  %s\n", m.Uid, m.Size, m.InternalDate.Format(time.RFC3339), m.Modseq,
		strings.Join(m.Flags, " "))
}

func search(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	req, ok := searchRequest(ctx, c, a, stderr)
	if !ok {
		return exUsage
	}

	return request(stderr, func() (*adminapi.SearchResult, *adminapi.Error, int, error) {
		resp, err := c.SearchMessagesWithResponse(ctx, a.args[0], req)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(res *adminapi.SearchResult) int {
		if a.asJSON {
			return printJSON(stdout, res)
		}

		for _, h := range res.Items {
			subject, from := "", ""
			if h.Envelope != nil {
				subject, from = h.Envelope.Subject, strings.Join(h.Envelope.From, ", ")
			}

			sayf(stdout, "%s  uidvalidity %d  uid %d  %s  %q  %s\n", h.Mailbox, h.Uidvalidity, h.Message.Uid,
				h.Message.InternalDate.Format(time.RFC3339), subject, from)
		}

		sayf(stdout, "%d hits, %d messages examined\n", len(res.Items), res.Scanned)

		if res.Next != nil {
			sayf(stdout, "more: -after %s\n", *res.Next)
		}

		return exOK
	})
}

// searchRequest builds the search from the flags; false on a bad flag.
func searchRequest(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stderr io.Writer) (adminapi.MessageSearch, bool) {
	req := adminapi.MessageSearch{From: optional(a.from), To: optional(a.to), Subject: optional(a.subject),
		MessageId: optional(a.messageID), Text: optional(a.text), After: optional(a.after), Limit: optInt(a.limit),
		Flags: optList(a.flags), NotFlags: optList(a.notFlags)}

	if a.mailbox != "" {
		id, _, ok := resolveMailbox(ctx, c, a.args[0], a.mailbox, stderr)
		if !ok {
			return req, false
		}

		req.MailboxId = &id
	}

	var err error
	if req.Since, err = optDate(a.since); err == nil {
		req.Before, err = optDate(a.before)
	}

	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return req, false
	}

	return req, true
}

// messageRef resolves <account> <mailbox> <uid> with the UIDVALIDITY.
func messageRef(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stderr io.Writer) (string, int64, int64, int, bool) {
	id, validity, code, ok := mailboxState(ctx, c, a, a.args[0], a.args[1], stderr)
	if !ok {
		return "", 0, 0, code, false
	}

	uid, err := strconv.ParseInt(a.args[2], 10, 64)
	if err != nil {
		sayln(stderr, "shardpostctl: invalid uid")
		return "", 0, 0, exUsage, false
	}

	return id, validity, uid, exOK, true
}

func messageShow(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	id, validity, uid, code, ok := messageRef(ctx, c, a, stderr)
	if !ok {
		return code
	}

	return request(stderr, func() (*adminapi.MessageDetail, *adminapi.Error, int, error) {
		resp, err := c.GetMessageWithResponse(ctx, a.args[0], id, uid, &adminapi.GetMessageParams{Uidvalidity: validity})
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(d *adminapi.MessageDetail) int {
		if a.asJSON {
			return printJSON(stdout, d)
		}

		sayf(stdout, "uidvalidity %d  guid %s\n", d.Uidvalidity, d.Message.Guid)
		printMeta(stdout, d.Message)
		sayln(stdout, "")
		sayf(stdout, "%s", d.Header)

		return exOK
	})
}

func messageExport(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	id, validity, uid, code, ok := messageRef(ctx, c, a, stderr)
	if !ok {
		return code
	}

	resp, err := c.ExportMessage(ctx, a.args[0], id, uid, &adminapi.ExportMessageParams{Uidvalidity: validity})
	if err != nil {
		return transportError(stderr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		parsed, _ := adminapi.ParseExportMessageResponse(resp)
		return failure(stderr, parsed.JSONDefault, resp.StatusCode)
	}

	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return transportError(stderr, err)
	}

	return exOK
}

// uidList parses "1,2,5".
func uidList(s string) ([]int64, error) {
	var out []int64

	for _, f := range strings.Split(s, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid uid %q", f)
		}

		out = append(out, n)
	}

	return out, nil
}

func flagsCmd(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
	id, validity, code, ok := mailboxState(ctx, c, a, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	uids, err := uidList(a.args[2])
	if err != nil {
		sayln(stderr, "shardpostctl:", err)
		return exUsage
	}

	req := adminapi.FlagRequest{Uidvalidity: validity, Uids: uids, Add: optList(a.add), Remove: optList(a.remove),
		UnchangedSince: optInt64(a.unchangedSince)}

	return request(stderr, func() (*adminapi.FlagResult, *adminapi.Error, int, error) {
		resp, err := c.SetMessageFlagsWithResponse(ctx, a.args[0], id, req)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(res *adminapi.FlagResult) int {
		if a.asJSON {
			return printJSON(stdout, res)
		}

		for _, m := range res.Updated {
			printMeta(stdout, m)
		}

		sayf(stdout, "%d changed, modified since: %v, missing: %v\n", len(res.Updated), res.Modified, res.Missing)

		return exOK
	})
}

func copyCmd(move bool) supportFunc {
	return func(ctx context.Context, c *adminapi.ClientWithResponses, a supportArgs, stdout, stderr io.Writer) int {
		id, validity, code, ok := mailboxState(ctx, c, a, a.args[0], a.args[1], stderr)
		if !ok {
			return code
		}

		dest, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[3], stderr)
		if !ok {
			return code
		}

		uids, err := uidList(a.args[2])
		if err != nil {
			sayln(stderr, "shardpostctl:", err)
			return exUsage
		}

		req := adminapi.CopyRequest{Uidvalidity: validity, Uids: uids, DestinationId: dest, Move: &move}

		return request(stderr, func() (*adminapi.CopyResult, *adminapi.Error, int, error) {
			resp, err := c.CopyMessagesWithResponse(ctx, a.args[0], id, req)
			if err != nil {
				return nil, nil, 0, err
			}

			return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
		}, func(res *adminapi.CopyResult) int {
			if a.asJSON {
				return printJSON(stdout, res)
			}

			sayf(stdout, "destination %s uidvalidity %d\n", res.DestinationId, res.DestinationUidvalidity)

			for _, m := range res.Copied {
				sayf(stdout, "%d -> %d\n", m.Uid, m.NewUid)
			}

			if len(res.Missing) > 0 {
				sayf(stdout, "missing: %v\n", res.Missing)
			}

			return exOK
		})
	}
}

func optInt(s string) *int {
	if n, err := strconv.Atoi(s); err == nil {
		return &n
	}

	return nil
}

func optInt64(s string) *int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return &n
	}

	return nil
}

func optList(s string) *[]string {
	if s == "" {
		return nil
	}

	l := strings.Split(s, ",")

	return &l
}

// optDate parses RFC 3339 or YYYY-MM-DD ("" = none).
func optDate(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}

	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t, nil
		}
	}

	return nil, fmt.Errorf("invalid date %q (RFC 3339 or YYYY-MM-DD)", s)
}

// readInput reads a script or message from -file or stdin.
func readInput(a supportArgs) ([]byte, error) {
	if a.scriptFile != "" {
		return os.ReadFile(a.scriptFile)
	}

	return io.ReadAll(io.LimitReader(a.stdin, 1<<20+1))
}
