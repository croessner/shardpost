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

// Delayed JMAP submissions (scaling plan B10):
//
//	submission list [-json] <account>          role diagnose
//	submission cancel [-json] <account> <id>   role support

const (
	verbList        = "list"
	verbCancel      = "cancel"
	cmdSubmission   = "submission"
	submissionUsage = "usage: shardpostctl submission list [-json] <account> | submission cancel [-json] <account> <id>"
)

// submissionCmd dispatches the submission commands.
func submissionCmd(client *adminapi.ClientWithResponses, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		sayln(stderr, submissionUsage)
		return exUsage
	}

	verb := args[0]

	fs := flag.NewFlagSet(cmdSubmission+" "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")

	rest, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return exUsage
	}

	ctx := context.Background()

	switch {
	case verb == verbList && len(rest) == 1:
		return submissionList(ctx, client, rest[0], *asJSON, stdout, stderr)
	case verb == verbCancel && len(rest) == 2:
		return submissionCancel(ctx, client, rest[0], rest[1], *asJSON, stdout, stderr)
	default:
		sayln(stderr, submissionUsage)
		return exUsage
	}
}

func submissionList(ctx context.Context, c *adminapi.ClientWithResponses, account string, asJSON bool, stdout, stderr io.Writer) int {
	return request(stderr, func() (*adminapi.SubmissionList, *adminapi.Error, int, error) {
		resp, err := c.ListSubmissionsWithResponse(ctx, account)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(list *adminapi.SubmissionList) int {
		if asJSON {
			return printJSON(stdout, list)
		}

		for _, s := range list.Items {
			sayf(stdout, "%s  %-8s release %s  attempts %d  %d bytes  %s -> %s\n", s.Id, s.State,
				s.Release.UTC().Format(time.RFC3339), s.Attempts, s.Size, s.MailFrom, strings.Join(s.RcptTo, ","))
		}

		return exOK
	})
}

func submissionCancel(ctx context.Context, c *adminapi.ClientWithResponses, account, id string, asJSON bool, stdout, stderr io.Writer) int {
	return request(stderr, func() (*adminapi.Submission, *adminapi.Error, int, error) {
		resp, err := c.CancelSubmissionWithResponse(ctx, account, id)
		if err != nil {
			return nil, nil, 0, err
		}

		return resp.JSON200, resp.JSONDefault, resp.StatusCode(), nil
	}, func(s *adminapi.Submission) int {
		if asJSON {
			return printJSON(stdout, s)
		}

		rows := [][2]string{{labelID, s.Id}, {cmdAccount, s.Account}, {labelState, string(s.State)},
			{"release", s.Release.UTC().Format(time.RFC3339)}, {labelAttempts, fmt.Sprint(s.Attempts)}}
		if s.CanceledBy != nil {
			rows = append(rows, [2]string{"canceled_by", *s.CanceledBy})
		}

		table(stdout, rows)

		return exOK
	})
}
