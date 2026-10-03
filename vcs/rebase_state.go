package vcs

import (
	"errors"
	"io/fs"
	"path/filepath"
)

func (r *Repo) rebaseStatePath() string {
	return filepath.Join(r.root, "rebase-state", "state.json")
}

func (r *Repo) writeRebaseState(st *rebaseState) error {
	return writeJSONAtomic(r.rebaseStatePath(), st)
}

func (r *Repo) rebaseInProgress() (*rebaseState, error) {
	var st rebaseState
	if err := readJSONFile(r.rebaseStatePath(), &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoRebase
		}
		return nil, err
	}
	if st.Onto == "" {
		return nil, ErrNoRebase
	}
	return &st, nil
}

func (r *Repo) clearRebaseState() error {
	return removeIfExists(r.rebaseStatePath())
}
