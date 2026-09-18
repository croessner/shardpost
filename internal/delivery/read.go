package delivery

import (
	"bytes"
	"errors"
	"io"

	"shardpost/internal/membudget"
	"shardpost/internal/model"
)

// ErrBusy means the process memory budget has no room for the message
// now; the sender should try again later.
var ErrBusy = errors.New("insufficient memory for the message")

// readStep is the unit in which a message is reserved and read.
const readStep = 1 << 20

// ReadMessage reads a message of at most limit bytes in steps of 1 MiB,
// each reserved against the process memory budget before it is read.
// release gives the reservations back (also after an error); the data
// must not be used after release. A longer message is model.ErrTooLarge.
func ReadMessage(r io.Reader, limit int64) (data []byte, release func(), err error) {
	var (
		buf      bytes.Buffer
		releases []func()
	)

	release = func() {
		for _, rel := range releases {
			rel()
		}

		releases = nil
	}

	for {
		if int64(buf.Len()) >= limit {
			// At the limit: one more byte decides, without reserving
			// another step the budget may not have.
			var one [1]byte
			if _, err := io.ReadFull(r, one[:]); err == nil {
				return nil, release, model.ErrTooLarge
			}

			return buf.Bytes(), release, nil
		}

		want := min(int64(readStep), limit-int64(buf.Len()))

		rel, err := membudget.Acquire(want)
		if err != nil {
			return nil, release, ErrBusy
		}

		releases = append(releases, rel)

		n, err := io.CopyN(&buf, r, want)
		if err == io.EOF || (err == nil && n < want) {
			return buf.Bytes(), release, nil
		}

		if err != nil {
			return nil, release, err
		}
	}
}
