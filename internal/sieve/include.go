package sieve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"shardpost/internal/model"
)

// IncludeLoader resolves "include" scripts: :personal from the user's
// ManageSieve script store, :global from a directory of <name>.sieve files.
// A nil user or an empty directory yields "no such script".
func IncludeLoader(u model.User, globalDir string) func(name string, global bool) ([]byte, bool, error) {
	return func(name string, global bool) ([]byte, bool, error) {
		if strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
			return nil, false, nil
		}

		if global {
			if globalDir == "" {
				return nil, false, nil
			}

			b, err := os.ReadFile(filepath.Join(globalDir, name+".sieve"))
			if errors.Is(err, os.ErrNotExist) {
				return nil, false, nil
			}

			return b, err == nil, err
		}

		if u == nil {
			return nil, false, nil
		}

		text, err := u.GetScript(name)
		if err != nil {
			if errors.Is(err, model.ErrNoScript) {
				return nil, false, nil
			}

			return nil, false, err
		}

		return []byte(text), true, nil
	}
}
