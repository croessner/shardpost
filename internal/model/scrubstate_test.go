package model_test

import (
	"testing"
	"time"

	"shardpost/internal/model"
)

func TestSetAccountScrub(t *testing.T) {
	ix := directory()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := model.AccountScrub{At: now, State: model.ScrubState{AccountSeq: 7, RegistryVersion: 42}, Complete: true}

	if err := model.RegisterAccount(ix, "alice", "", "fsid", now); err != nil {
		t.Fatal(err)
	}

	if err := model.SetAccountScrub(ix, "alice", rec); err != nil {
		t.Fatal(err)
	}

	e, _ := entryOf(t, ix, "alice")

	got, ok := e.ScrubOf()
	if !ok || got != rec {
		t.Fatalf("scrub record %+v %v", got, ok)
	}

	if e.State != model.AccountActive || e.Cluster != "fsid" || !e.Created.Equal(now) {
		t.Fatalf("the record changed the entry: %+v", e)
	}

	// A new registration (the account was created again) drops it.
	if err := model.RegisterAccount(ix, "alice", "", "fsid", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if e, _ = entryOf(t, ix, "alice"); len(e.Scrub) != 0 {
		t.Fatalf("scrub record survived a registration: %s", e.Scrub)
	}
}

// TestSetAccountScrubCreatesNoEntry: the directory holds only accounts
// with a registry; a scrub record never adds one.
func TestSetAccountScrubCreatesNoEntry(t *testing.T) {
	ix := directory()
	rec := model.AccountScrub{At: time.Now(), Complete: true}

	for _, name := range []string{"ghost", "~admin"} {
		if err := model.SetAccountScrub(ix, name, rec); err != nil {
			t.Fatal(err)
		}

		if _, ok := entryOf(t, ix, name); ok {
			t.Fatalf("a scrub record created an entry for %s", name)
		}
	}
}

func TestScrubIdle(t *testing.T) {
	at := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	state := model.ScrubState{AccountSeq: 10, RegistryVersion: 99}
	last := model.AccountScrub{At: at.Add(-24 * time.Hour), State: state, Complete: true}
	week := 7 * 24 * time.Hour

	if !model.ScrubIdle(last, true, state, "", at, week) {
		t.Fatal("an unchanged, clean account is not idle")
	}

	cases := map[string]func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration){
		"never scrubbed": func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration) {
			return model.AccountScrub{}, false, state, "", week
		},
		"incomplete": func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration) {
			l := last
			l.Complete = false

			return l, true, state, "", week
		},
		"findings": func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration) {
			l := last
			l.Findings = 1

			return l, true, state, "", week
		},
		"account changed": func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration) {
			return last, true, model.ScrubState{AccountSeq: 11, RegistryVersion: 99}, "", week
		},
		"registry changed": func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration) {
			return last, true, model.ScrubState{AccountSeq: 10, RegistryVersion: 100}, "", week
		},
		"pending": func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration) {
			return last, true, state, "orphan candidates", week
		},
		"interval over": func() (model.AccountScrub, bool, model.ScrubState, string, time.Duration) {
			return last, true, state, "", 24 * time.Hour
		},
	}

	for name, c := range cases {
		l, ok, now, pending, interval := c()
		if model.ScrubIdle(l, ok, now, pending, at, interval) {
			t.Errorf("%s: skipped", name)
		}
	}
}
