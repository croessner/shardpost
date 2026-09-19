package migrate

import (
	"bytes"
	"context"
	"fmt"

	"shardpost/internal/adminapi"
)

// Target is the ShardPost side of a migration: the admin import API.
type Target interface {
	Open(ctx context.Context, account, session string) (string, error)
	Close(ctx context.Context) error
	Mailboxes(ctx context.Context) ([]adminapi.ImportMailboxState, error)
	PutMailbox(ctx context.Context, spec adminapi.ImportMailboxSpec) (*adminapi.ImportMailboxState, error)
	DeleteMailbox(ctx context.Context, id string) error
	Inventory(ctx context.Context, id string, after int64) (*adminapi.ImportInventory, error)
	Import(ctx context.Context, id string, parts []adminapi.ImportPart) (*adminapi.ImportResult, error)
	Changes(ctx context.Context, id string, flags []adminapi.ImportFlagChange, expunge []int64) (*adminapi.ImportMailboxState, error)
	Finalize(ctx context.Context, id string, uidNext uint32, highestModSeq uint64) (*adminapi.ImportMailboxState, error)
	PutACL(ctx context.Context, id string, entries map[string]string) error
	PutSieve(ctx context.Context, scripts []adminapi.ImportSieveScript, active string) error
}

// APIError is a failed admin API call.
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("admin API: HTTP %d", e.Status)
	}

	return fmt.Sprintf("admin API: %s: %s", e.Code, e.Msg)
}

// apiError builds the error of an unexpected answer.
func apiError(status int, doc *adminapi.Error) error {
	if doc == nil {
		return &APIError{Status: status}
	}

	return &APIError{Status: status, Code: string(doc.Code), Msg: doc.Message}
}

// APITarget is Target over the generated admin API client.
type APITarget struct {
	C       *adminapi.ClientWithResponses
	account string
	session string
}

// Open implements Target.
func (t *APITarget) Open(ctx context.Context, account, session string) (string, error) {
	var body adminapi.ImportSessionRequest
	if session != "" {
		body.Session = &session
	}

	resp, err := t.C.OpenImportWithResponse(ctx, account, body)
	if err != nil {
		return "", err
	}

	if resp.JSON200 == nil {
		return "", apiError(resp.StatusCode(), resp.JSONDefault)
	}

	t.account, t.session = account, resp.JSON200.Session

	return t.session, nil
}

// Close implements Target.
func (t *APITarget) Close(ctx context.Context) error {
	resp, err := t.C.CloseImportWithResponse(ctx, t.account, &adminapi.CloseImportParams{XImportSession: t.session})
	if err != nil {
		return err
	}

	if resp.StatusCode() != 204 {
		return apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return nil
}

// Mailboxes implements Target.
func (t *APITarget) Mailboxes(ctx context.Context) ([]adminapi.ImportMailboxState, error) {
	resp, err := t.C.ListImportMailboxesWithResponse(ctx, t.account, &adminapi.ListImportMailboxesParams{XImportSession: t.session})
	if err != nil {
		return nil, err
	}

	if resp.JSON200 == nil {
		return nil, apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200.Mailboxes, nil
}

// PutMailbox implements Target.
func (t *APITarget) PutMailbox(ctx context.Context, spec adminapi.ImportMailboxSpec) (*adminapi.ImportMailboxState, error) {
	resp, err := t.C.PutImportMailboxWithResponse(ctx, t.account, &adminapi.PutImportMailboxParams{XImportSession: t.session}, spec)
	if err != nil {
		return nil, err
	}

	if resp.JSON200 == nil {
		return nil, apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200, nil
}

// DeleteMailbox implements Target.
func (t *APITarget) DeleteMailbox(ctx context.Context, id string) error {
	resp, err := t.C.DeleteImportMailboxWithResponse(ctx, t.account, id, &adminapi.DeleteImportMailboxParams{XImportSession: t.session})
	if err != nil {
		return err
	}

	if resp.StatusCode() != 204 {
		return apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return nil
}

// Inventory implements Target.
func (t *APITarget) Inventory(ctx context.Context, id string, after int64) (*adminapi.ImportInventory, error) {
	resp, err := t.C.ListImportMessagesWithResponse(ctx, t.account, id, &adminapi.ListImportMessagesParams{XImportSession: t.session, After: &after})
	if err != nil {
		return nil, err
	}

	if resp.JSON200 == nil {
		return nil, apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200, nil
}

// Import implements Target.
func (t *APITarget) Import(ctx context.Context, id string, parts []adminapi.ImportPart) (*adminapi.ImportResult, error) {
	var body bytes.Buffer

	ct, err := adminapi.WriteImportBody(&body, parts)
	if err != nil {
		return nil, err
	}

	resp, err := t.C.ImportMessagesWithBodyWithResponse(ctx, t.account, id, &adminapi.ImportMessagesParams{XImportSession: t.session}, ct, &body)
	if err != nil {
		return nil, err
	}

	if resp.JSON200 == nil {
		return nil, apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200, nil
}

// Changes implements Target.
func (t *APITarget) Changes(ctx context.Context, id string, flags []adminapi.ImportFlagChange, expunge []int64) (*adminapi.ImportMailboxState, error) {
	resp, err := t.C.ImportChangesWithResponse(ctx, t.account, id, &adminapi.ImportChangesParams{XImportSession: t.session},
		adminapi.ImportChanges{Flags: &flags, Expunge: &expunge})
	if err != nil {
		return nil, err
	}

	if resp.JSON200 == nil {
		return nil, apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200, nil
}

// Finalize implements Target.
func (t *APITarget) Finalize(ctx context.Context, id string, uidNext uint32, highestModSeq uint64) (*adminapi.ImportMailboxState, error) {
	resp, err := t.C.FinalizeImportMailboxWithResponse(ctx, t.account, id, &adminapi.FinalizeImportMailboxParams{XImportSession: t.session},
		adminapi.ImportFinalize{UidNext: int64(uidNext), HighestModseq: int64(highestModSeq)})
	if err != nil {
		return nil, err
	}

	if resp.JSON200 == nil {
		return nil, apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200, nil
}

// PutACL implements Target.
func (t *APITarget) PutACL(ctx context.Context, id string, entries map[string]string) error {
	resp, err := t.C.PutImportACLWithResponse(ctx, t.account, id, &adminapi.PutImportACLParams{XImportSession: t.session}, adminapi.ImportACL{Entries: entries})
	if err != nil {
		return err
	}

	return noContent(resp.StatusCode(), resp.JSONDefault)
}

// PutSieve implements Target.
func (t *APITarget) PutSieve(ctx context.Context, scripts []adminapi.ImportSieveScript, active string) error {
	resp, err := t.C.PutImportSieveWithResponse(ctx, t.account, &adminapi.PutImportSieveParams{XImportSession: t.session}, adminapi.ImportSieve{Scripts: scripts, Active: active})
	if err != nil {
		return err
	}

	if resp.StatusCode() != 204 {
		return apiError(resp.StatusCode(), resp.JSONDefault)
	}

	return nil
}

// MetadataTarget is a target that also takes mailbox annotations.
type MetadataTarget interface {
	PutMetadata(ctx context.Context, id string, entries map[string]string) error
}

// PutMetadata implements MetadataTarget.
func (t *APITarget) PutMetadata(ctx context.Context, id string, entries map[string]string) error {
	resp, err := t.C.PutImportMetadataWithResponse(ctx, t.account, id, &adminapi.PutImportMetadataParams{XImportSession: t.session},
		adminapi.ImportMetadata{Entries: entries})
	if err != nil {
		return err
	}

	return noContent(resp.StatusCode(), resp.JSONDefault)
}

// noContent is nil for 204, the API error otherwise.
func noContent(status int, doc *adminapi.Error) error {
	if status != 204 {
		return apiError(status, doc)
	}

	return nil
}
