package interp

import (
	"bytes"
	"encoding/gob"
	"io"
)

type savedOptions struct {
	Config       map[string]string
	MaxActions   int
	MaxRedirects int

	MaxVariableCount   int
	MaxVariableNameLen int
	MaxVariableLen     int
	SubAddressSep      string
}

type savedScript struct {
	Extensions []string
	Options    savedOptions
	Cmds       []Cmd
	Globals    []string
}

// GobEncode lets a Script travel inside other gob values (an included
// script inside CmdInclude).
func (s *Script) GobEncode() ([]byte, error) { return s.Save() }

func (s *Script) GobDecode(b []byte) error {
	r, err := Restore(b)
	if err != nil {
		return err
	}
	*s = *r
	return nil
}

func (s Script) SaveTo(w io.Writer) error {
	saved := savedScript{
		Extensions: make([]string, 0, len(s.extensions)),
		Options: savedOptions{
			Config: s.opts.Config, MaxActions: s.opts.MaxActions,
			MaxRedirects:       s.opts.MaxRedirects,
			MaxVariableCount:   s.opts.MaxVariableCount,
			MaxVariableNameLen: s.opts.MaxVariableNameLen,
			MaxVariableLen:     s.opts.MaxVariableLen,
			SubAddressSep:      s.opts.SubAddressSep,
		},
		Cmds: s.cmd,
	}

	for ext := range s.extensions {
		saved.Extensions = append(saved.Extensions, ext)
	}
	for g := range s.globals {
		saved.Globals = append(saved.Globals, g)
	}

	return gob.NewEncoder(w).Encode(saved)
}

func (s Script) Save() ([]byte, error) {
	var buf bytes.Buffer
	err := s.SaveTo(&buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RestoreFrom(r io.Reader) (*Script, error) {
	var saved savedScript
	err := gob.NewDecoder(r).Decode(&saved)
	if err != nil {
		return nil, err
	}

	restored := Script{
		extensions: make(map[string]struct{}, len(saved.Extensions)),
		opts: &Options{
			Config: saved.Options.Config, MaxActions: saved.Options.MaxActions,
			MaxRedirects:       saved.Options.MaxRedirects,
			MaxVariableCount:   saved.Options.MaxVariableCount,
			MaxVariableNameLen: saved.Options.MaxVariableNameLen,
			MaxVariableLen:     saved.Options.MaxVariableLen,
			SubAddressSep:      saved.Options.SubAddressSep,
		},
		cmd: saved.Cmds,
	}
	for _, ext := range saved.Extensions {
		restored.extensions[ext] = struct{}{}
	}
	if len(saved.Globals) > 0 {
		restored.globals = map[string]bool{}
		for _, g := range saved.Globals {
			restored.globals[g] = true
		}
	}
	return &restored, nil
}

func Restore(blob []byte) (*Script, error) {
	return RestoreFrom(bytes.NewReader(blob))
}
