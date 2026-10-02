package config

import (
	"fmt"

	"shardpost/internal/charset"
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
	// while the cluster mark ("shardpostctl fts layout3 enable") is set;
	// the layout of an existing index is the one its id map records.
	Layout int `mapstructure:"layout" validate:"oneof=2 3"`
	// ShardsMin is the number of posting, dictionary and header objects
	// of a fresh layout 3 index without a size hint; ShardsMax bounds
	// what a reshard chooses.
	ShardsMin int `mapstructure:"shards_min" validate:"gte=1,lte=4096"`
	ShardsMax int `mapstructure:"shards_max" validate:"gte=1,lte=4096"`
	// ShardTargetKeys is the number of keys a layout 3 object should hold
	// (T): a shard with half as many again asks for a reshard.
	ShardTargetKeys int `mapstructure:"shard_target_keys" validate:"gte=10000,lte=1000000"`
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
}

// shardTargetWarn is the target key count above which the configuration
// is accepted with a warning: Ceph warns about omap objects of more than
// 200000 keys unless osd_deep_scrub_large_omap_object_key_threshold is
// raised.
const shardTargetWarn = 200000

// ShardTargetWarning is the warning the server logs at startup for a
// shard target above Ceph's large omap threshold ("" for none).
func (f FTS) ShardTargetWarning() string {
	if f.ShardTargetKeys <= shardTargetWarn {
		return ""
	}

	return fmt.Sprintf("fts.shard_target_keys %d is above the 200000 keys at which Ceph reports large omap objects; "+
		"raise osd_deep_scrub_large_omap_object_key_threshold accordingly", f.ShardTargetKeys)
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

	return nil
}
