package migrate

import (
	"errors"
	"unicode/utf8"

	"shardpost/internal/mutf7"
)

// decodeMUTF7 decodes a mailbox name of the source server, in modified
// UTF-7 (RFC 3501 5.1.3). A name that is already UTF-8 (UTF8=ACCEPT) is
// returned unchanged. Forms that are not canonical are accepted: the name
// is the source's, and it is stored in UTF-8 anyway.
func decodeMUTF7(s string) (string, error) {
	if !isASCII(s) {
		if !utf8.ValidString(s) {
			return "", errors.New("mailbox name is neither modified UTF-7 nor UTF-8")
		}

		return s, nil
	}

	return mutf7.Decode(s)
}
