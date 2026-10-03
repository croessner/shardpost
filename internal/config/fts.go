package config

import (
	"fmt"
	"path"

	"shardpost/internal/charset"
	"shardpost/internal/window"
)

// FTS configures how message text is prepared for the full-text index.
// The same preparation answers IMAP SEARCH without the index and the
// cached preview, so a change applies to new messages at once and to
// existing ones after an fts_rebuild.
type FTS struct {
	// FallbackCharset is the charset text is read in that is not valid
	// UTF-8 and has no charset label, is labelled US-ASCII or names a
	// charset that is not known.
	FallbackCharset string `mapstructure:"fallback_charset"`
	// HTML is what is made of a text/html part: "text" indexes its
	// visible text, "raw" the markup as it is, "skip" leaves it out.
	HTML string `mapstructure:"html" validate:"oneof=text raw skip"`
	// IMAPSearch is how IMAP SEARCH matches text: "substring" as RFC 9051
	// specifies, the index narrowing the candidates and each candidate
	// checked against the message; "word" lets the index answer SUBJECT,
	// BODY, TEXT and HEADER Subject alone, by word, stem and in-word
	// substring, without reading messages.
	IMAPSearch string `mapstructure:"imap_search" validate:"oneof=substring word"`
	// VerifyParallel bounds the messages one IMAP SEARCH checks against
	// its text keys at the same time.
	VerifyParallel int `mapstructure:"verify_parallel" validate:"gte=1,lte=64"`

	// Layout is the object layout of indexes created from now on: 2 the
	// fixed shards "fts/", "ftst/", "ftsh/" of every version before 0.45.0,
	// 3 the adaptive generations "ftx/<gen>/…". Layout 3 is used only
	// while the cluster mark ("shardpostctl fts layout3 enable") is set:
	// the default 3 (since 0.46.0) creates layout 2 indexes in a cluster
	// without the mark, and 2 keeps layout 2 even with the mark. The
	// layout of an existing index is the one its id map records.
	Layout int `mapstructure:"layout" validate:"oneof=2 3"`
	// ShardsMin is the number of posting, dictionary and header objects
	// of a fresh layout 3 index without a size hint; ShardsMax bounds
	// the posting and header objects a reshard chooses (S and H),
	// DictShardsMax the dictionary objects (D).
	ShardsMin     int `mapstructure:"shards_min" validate:"gte=1,lte=4096"`
	ShardsMax     int `mapstructure:"shards_max" validate:"gte=1,lte=4096"`
	DictShardsMax int `mapstructure:"dict_shards_max" validate:"gte=1,lte=4096"`
	// ShardTargetKeys is the number of keys a layout 3 posting or header
	// object should hold (T_p), DictTargetKeys the number a dictionary
	// object should hold (T_d): an object with half as many again asks
	// for a reshard.
	ShardTargetKeys int `mapstructure:"shard_target_keys" validate:"gte=10000,lte=1000000"`
	DictTargetKeys  int `mapstructure:"dict_target_keys" validate:"gte=10000,lte=1000000"`
	// ConvertBufferKeys is the least number of keys a conversion (P5)
	// holds before it writes them; it may hold 1024 per target object
	// when that is more. ConvertWriters is the number of writes it has
	// in flight.
	ConvertBufferKeys int `mapstructure:"convert_buffer_keys" validate:"gte=16384,lte=4194304"`
	ConvertWriters    int `mapstructure:"convert_writers" validate:"gte=1,lte=64"`
	// StatsTestAccounts are the accounts (path.Match patterns on the
	// lower-cased account, e.g. "bench-*") whose index terms
	// "shardpostctl fts stats" may classify; for every other account it
	// only counts keys.
	StatsTestAccounts []string `mapstructure:"stats_test_accounts" validate:"dive,required"`
	// AutoReshard runs the fts_due worker, which plans fts_layout jobs
	// for the accounts whose shards outgrew ShardTargetKeys.
	AutoReshard bool `mapstructure:"auto_reshard"`
	// FrozenWaitSeconds is how long one indexing waits, once, for a
	// conversion that froze the account's posting objects to finish
	// before it gives up with a temporary failure.
	FrozenWaitSeconds int `mapstructure:"frozen_wait_seconds" validate:"gte=0,lte=60"`
	// ImportReshardSyncSeconds is how long the import reshard endpoint
	// works before it answers 202 and goes on in the background.
	ImportReshardSyncSeconds int `mapstructure:"import_reshard_sync_seconds" validate:"gte=1,lte=90"`
	// DictMinTerms is reserved for the dictionary threshold (B14f); it
	// has no effect yet.
	DictMinTerms int `mapstructure:"dict_min_terms" validate:"gte=0"`
	// DictFilter is the identifier filter of the substring dictionary of
	// index generations created from now on (B14h): "none" lists every
	// body term under each of its trigrams (exact substring search, the
	// default), "ids" gives a body term with letters and digits of at
	// least DictIDMinRunes runes one start entry instead, so that it is
	// found as a whole word and by its start but not as an inner part.
	// "ids" takes effect only while the cluster mark ("shardpostctl fts
	// dictfilter enable") is set; existing generations change with
	// fts_layout.
	DictFilter string `mapstructure:"dict_filter" validate:"oneof=none ids"`
	// DictIDMinRunes is the least length in runes of an identifier.
	DictIDMinRunes int `mapstructure:"dict_id_min_runes" validate:"gte=13,lte=40"`
	// AttachedHeaders is which header fields of attached messages the
	// body text has: "all" every field (the default), "nosig" all but
	// the signature and trace fields (DKIM-Signature, ARC-*,
	// Authentication-Results, Received, Received-SPF, X-*-Signature).
	// IMAP SEARCH BODY and TEXT check messages by it at once; an index
	// generation is built with it while the cluster mark of the
	// dictionary filter is set, existing ones change with fts_rebuild.
	AttachedHeaders string `mapstructure:"attached_headers" validate:"oneof=all nosig"`

	// The online runs of fts_layout and fts_rebuild (stage B14i): they
	// need the cluster mark ("shardpostctl fts online enable") and run
	// without the account's maintenance lock.
	//
	// OnlineBuild is what a plan of fts_layout or fts_rebuild without
	// "online" gets: "auto" online while the mark is set, "off" offline
	// (under the lock, as before 0.51.0).
	OnlineBuild string `mapstructure:"online_build" validate:"oneof=auto off"`
	// ReshardWindow ("HH:MM-HH:MM", over midnight allowed; empty: none)
	// in the time zone ReshardWindowTZ (an IANA name) is when the fts_due
	// worker and a batch of fts_layout or fts_rebuild start the run of an
	// account; a run an operator plans for one account starts at once.
	ReshardWindow   string `mapstructure:"reshard_window"`
	ReshardWindowTZ string `mapstructure:"reshard_window_tz"`
	// BuildMaxRunning bounds the jobs the fts_due worker has planned
	// that have not ended (with a window).
	BuildMaxRunning int `mapstructure:"build_max_running" validate:"gte=1,lte=64"`
	// BuildCatchupRounds are the switch attempts of an online run before
	// it holds new id allocations for BuildHoldMS milliseconds (0: never;
	// the run fails after its rounds instead).
	BuildCatchupRounds int `mapstructure:"build_catchup_rounds" validate:"gte=1,lte=50"`
	BuildHoldMS        int `mapstructure:"build_hold_ms" validate:"gte=0,lte=5000"`
	// BuildMsPerMessageCopy and BuildMsPerMessageReindex estimate how
	// long a run takes per document of the account (copy: fts_layout;
	// reindex: fts_rebuild), to tell whether it fits into the window.
	BuildMsPerMessageCopy    float64 `mapstructure:"build_ms_per_message_copy" validate:"gte=0.1,lte=1000"`
	BuildMsPerMessageReindex float64 `mapstructure:"build_ms_per_message_reindex" validate:"gte=1,lte=10000"`
	// ReindexBatchDocs and ReindexBatchBytes bound a batch of messages an
	// online rebuild indexes at once.
	ReindexBatchDocs  int `mapstructure:"reindex_batch_docs" validate:"gte=50,lte=5000"`
	ReindexBatchBytes int `mapstructure:"reindex_batch_bytes" validate:"gte=1048576,lte=67108864"`
}

// Values of fts.online_build.
const (
	OnlineBuildAuto = "auto"
	OnlineBuildOff  = "off"
)

// Window is the reshard window of fts.reshard_window and
// fts.reshard_window_tz.
func (f FTS) Window() (window.Window, error) {
	w, err := window.Parse(f.ReshardWindow, f.ReshardWindowTZ)
	if err != nil {
		return w, fmt.Errorf("fts.reshard_window %q (fts.reshard_window_tz %q): %w", f.ReshardWindow, f.ReshardWindowTZ, err)
	}

	return w, nil
}

// DictIDFilter is the identifier length of the dictionary filter that
// fts.dict_filter asks for: fts.dict_id_min_runes with "ids", 0 with
// "none".
func (f FTS) DictIDFilter() int {
	if f.DictFilter != "ids" {
		return 0
	}

	return f.DictIDMinRunes
}

// shardTargetWarn is the target key count above which the configuration
// is accepted with a warning: an object grows to 1.5 times its target
// before a writer asks for a reshard, and Ceph warns about omap objects
// of more than 200000 keys unless
// osd_deep_scrub_large_omap_object_key_threshold is raised.
const shardTargetWarn = 133000

// Keys of the targets (defaults, warnings).
const (
	keyShardTargetKeys = "fts.shard_target_keys"
	keyDictTargetKeys  = "fts.dict_target_keys"
)

// ShardTargetWarnings are the warnings the server logs at startup for
// targets whose objects may pass Ceph's large omap threshold before a
// reshard (none: nil).
func (f FTS) ShardTargetWarnings() []string {
	var out []string

	for _, t := range []struct {
		key string
		n   int
	}{{keyShardTargetKeys, f.ShardTargetKeys}, {keyDictTargetKeys, f.DictTargetKeys}} {
		if t.n > shardTargetWarn {
			out = append(out, fmt.Sprintf("%s %d is above %d: an object grows to 1.5 times its target (%d keys) before a reshard, "+
				"beyond the 200000 keys at which Ceph reports large omap objects; raise osd_deep_scrub_large_omap_object_key_threshold accordingly",
				t.key, t.n, shardTargetWarn, t.n*3/2))
		}
	}

	return out
}

// StatsTestAccount reports whether "fts stats" may classify the terms
// of account (the lower-cased canonical account).
func (f FTS) StatsTestAccount(account string) bool {
	for _, p := range f.StatsTestAccounts {
		if ok, err := path.Match(p, account); err == nil && ok {
			return true
		}
	}

	return false
}

// IMAPWordSearch reports whether IMAP SEARCH uses word semantics.
func (f FTS) IMAPWordSearch() bool { return f.IMAPSearch == "word" }

// Validate refuses a fallback charset that is unknown or cannot read
// 8-bit text, and shard bounds that contradict each other.
func (f FTS) Validate() error {
	if _, err := charset.Fallback(f.FallbackCharset); err != nil {
		return fmt.Errorf("fts.fallback_charset: %w", err)
	}

	if f.ShardsMax < f.ShardsMin {
		return fmt.Errorf("fts.shards_max (%d) must not be below fts.shards_min (%d)", f.ShardsMax, f.ShardsMin)
	}

	if f.DictShardsMax < f.ShardsMin {
		return fmt.Errorf("fts.dict_shards_max (%d) must not be below fts.shards_min (%d)", f.DictShardsMax, f.ShardsMin)
	}

	for _, p := range f.StatsTestAccounts {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("fts.stats_test_accounts: pattern %q: %w", p, err)
		}
	}

	_, err := f.Window()

	return err
}
