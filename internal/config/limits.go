package config

// Limits bound how far one account can grow the omap objects that hold
// its state (see model.Limits for what each one counts). 0 means
// unlimited. The defaults are far above what accounts reach in normal
// use; they only stop an account that runs away (a script creating
// mailboxes or keywords in a loop) before its objects slow down the OSD
// that serves them.
type Limits struct {
	// Mailboxes bounds the mailboxes of an account. INBOX is never
	// refused.
	Mailboxes int64 `mapstructure:"mailboxes" env:"LIMITS_MAILBOXES" validate:"gte=0"`
	// MailboxNames bounds the distinct names an account ever created a
	// mailbox under (the registry remembers each one's UIDVALIDITY for
	// good).
	MailboxNames int64 `mapstructure:"mailbox_names" env:"LIMITS_MAILBOX_NAMES" validate:"gte=0"`
	// KeywordsPerMailbox bounds the user keywords one mailbox knows.
	KeywordsPerMailbox int64 `mapstructure:"keywords_per_mailbox" env:"LIMITS_KEYWORDS_PER_MAILBOX" validate:"gte=0"`
	// MetadataEntries bounds the METADATA entries of an account.
	MetadataEntries int64 `mapstructure:"metadata_entries" env:"LIMITS_METADATA_ENTRIES" validate:"gte=0"`
	// SharedPointers bounds the mailboxes of other accounts shared with
	// an account (the pointers ACL grants write into its registry).
	SharedPointers int64 `mapstructure:"shared_pointers" env:"LIMITS_SHARED_POINTERS" validate:"gte=0"`
}

// The defaults of Limits.
const (
	defaultLimitMailboxes       = 10000
	defaultLimitMailboxNames    = 50000
	defaultLimitKeywords        = 1000
	defaultLimitMetadataEntries = 1000
	defaultLimitSharedPointers  = 10000
)
