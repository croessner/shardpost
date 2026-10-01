package main

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"shardpost/internal/adminapi"
)

// Provisioning (role provision):
//
//	mailbox create [-special-use \Archive] <account> <name>
//	mailbox rename [-if-match E] <account> <mailbox> <new name>
//	mailbox subscribe|unsubscribe [-if-match E] <account> <mailbox>
//	acl set [-mode replace|add|remove] [-if-match E] <account> <mailbox> <identifier> <rights>
//	acl delete [-if-match E] <account> <mailbox> <identifier>
//
// Repair (role repair):
//
//	account repair <account>
//
// Changes print the new state like the matching show command and the
// ETag to pass as -if-match next time.

// provisionCommand returns a provisioning command and its arity.
func provisionCommand(verb string) (diagFunc, int) {
	switch verb {
	case "mailbox create":
		return mailboxCreate, 2
	case "mailbox rename":
		return mailboxRename, 3
	case "mailbox subscribe":
		return mailboxSubscribe(true), 2
	case "mailbox unsubscribe":
		return mailboxSubscribe(false), 2
	case "acl set":
		return aclSet, 4
	case "acl delete":
		return aclDelete, 3
	case "account repair":
		return accountRepair, 1
	default:
		return nil, 0
	}
}

// accountDirectory shows the account's directory entry (role diagnose).
func accountDirectory(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	resp, err := c.GetAccountDirectoryWithResponse(ctx, a.args[0])
	if err != nil {
		return transportError(stderr, err)
	}

	st := resp.JSON200
	if st == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	if a.asJSON {
		return printJSON(stdout, st)
	}

	rows := [][2]string{{cmdAccount, st.Account}, {"entry", entryState(st.Entry)}}
	if st.Entry != nil {
		rows = append(rows, [2]string{"entry created", st.Entry.Created.Format(time.RFC3339)}, [2]string{"last scrub", scrubState(st.Entry.Scrub)})
	}

	backfill := "(none)"
	if st.Backfill != nil {
		backfill = *st.Backfill
	}

	rows = append(rows, [2]string{"registry", strconv.FormatBool(st.Registry)},
		[2]string{"directory", strconv.Itoa(st.Shards) + " shards, " + st.Phase}, [2]string{"backfill", backfill})
	table(stdout, rows)

	return exOK
}

// entryState is a directory entry in one word (and its session).
func entryState(e *adminapi.DirectoryEntry) string {
	if e == nil {
		return "(no entry)"
	}

	if e.Session != nil {
		return string(e.State) + " (" + *e.Session + ")"
	}

	return string(e.State)
}

// accountRepair repairs the account's directory entry (role repair).
func accountRepair(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	resp, err := c.RepairAccountDirectoryWithResponse(ctx, a.args[0])
	if err != nil {
		return transportError(stderr, err)
	}

	rep := resp.JSON200
	if rep == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	if a.asJSON {
		return printJSON(stdout, rep)
	}

	table(stdout, [][2]string{{cmdAccount, rep.Account}, {"action", string(rep.Action)},
		{"entry before", entryState(rep.Before)}, {"entry after", entryState(rep.After)}})

	return exOK
}

func mailboxCreate(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	req := adminapi.MailboxCreate{Name: a.args[1]}

	if a.specialUse != "" {
		uses := strings.Split(a.specialUse, ",")
		req.SpecialUse = &uses
	}

	resp, err := c.CreateMailboxWithResponse(ctx, a.args[0], req)
	if err != nil {
		return transportError(stderr, err)
	}

	m := resp.JSON201
	if m == nil {
		m = resp.JSON200
	}

	if m == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printChanged(stdout, a, m, resp.HTTPResponse.Header.Get("ETag"))
}

// printChanged prints a changed mailbox and its ETag.
func printChanged(stdout io.Writer, a diagArgs, m *adminapi.MailboxSummary, etag string) int {
	if a.asJSON {
		return printJSON(stdout, m)
	}

	printMailboxes(stdout, []adminapi.MailboxSummary{*m}, nil)
	sayln(stdout, "etag "+etag)

	return exOK
}

// updateMailbox sends a PATCH for a mailbox given by name or id.
func updateMailbox(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer, req adminapi.MailboxUpdate) int {
	id, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	resp, err := c.UpdateMailboxWithResponse(ctx, a.args[0], id, &adminapi.UpdateMailboxParams{IfMatch: optional(a.ifMatch)}, req)
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printChanged(stdout, a, resp.JSON200, resp.HTTPResponse.Header.Get("ETag"))
}

func mailboxRename(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	return updateMailbox(ctx, c, a, stdout, stderr, adminapi.MailboxUpdate{Name: &a.args[2]})
}

func mailboxSubscribe(subscribed bool) diagFunc {
	return func(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
		return updateMailbox(ctx, c, a, stdout, stderr, adminapi.MailboxUpdate{Subscribed: &subscribed})
	}
}

func aclSet(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	id, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	mode := adminapi.ACLChangeMode(a.mode)

	resp, err := c.SetACLEntryWithResponse(ctx, a.args[0], id, a.args[2], &adminapi.SetACLEntryParams{IfMatch: optional(a.ifMatch)},
		adminapi.ACLChange{Rights: a.args[3], Mode: &mode})
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printACL(stdout, a, resp.JSON200, resp.HTTPResponse.Header.Get("ETag"))
}

func aclDelete(ctx context.Context, c *adminapi.ClientWithResponses, a diagArgs, stdout, stderr io.Writer) int {
	id, code, ok := resolveMailbox(ctx, c, a.args[0], a.args[1], stderr)
	if !ok {
		return code
	}

	resp, err := c.DeleteACLEntryWithResponse(ctx, a.args[0], id, a.args[2], &adminapi.DeleteACLEntryParams{IfMatch: optional(a.ifMatch)})
	if err != nil {
		return transportError(stderr, err)
	}

	if resp.JSON200 == nil {
		return failure(stderr, resp.JSONDefault, resp.StatusCode())
	}

	return printACL(stdout, a, resp.JSON200, resp.HTTPResponse.Header.Get("ETag"))
}
