package model

import "strings"

// ServerManagedPrefix is the part of the private METADATA namespace the
// server keeps for itself: the JMAP identities and the JMAP vacation
// response live there. Clients may read the entries; only the server
// writes them, because it checks what goes in and derives other state
// from it (the vacation reply at delivery).
const ServerManagedPrefix = "/private/vendor/shardpost/jmap/"

// ServerManagedEntry reports whether a METADATA entry is kept by the
// server. Entry names are compared without regard to case (RFC 5464 3.2).
func ServerManagedEntry(entry string) bool {
	return strings.HasPrefix(strings.ToLower(entry), ServerManagedPrefix)
}

// JMAPVacationScript is the Sieve script name the JMAP VacationResponse
// keeps a copy of itself under, for ManageSieve clients to show.
const JMAPVacationScript = "jmap-vacation"

// ReservedScript reports whether a script name belongs to the server:
// clients may read the script, not change it, and an export or a
// migration does not carry it (it follows from the account's metadata).
func ReservedScript(name string) bool { return name == JMAPVacationScript }

// The bounds of a Sieve script, which the RADOS store, ManageSieve, the
// admin API and the archive enforce and JMAP advertises: the name in bytes
// (512, the room RFC 9661 asks for: 128 characters of up to four bytes)
// and the text.
const (
	MaxScriptName = 512
	MaxScriptSize = 256 << 10
)
