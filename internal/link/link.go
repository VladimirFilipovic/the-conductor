// Package link manages the folder-link: a per-repo .conductor/config.json
// pointing a working tree at a project/environment/service. Identity pointers
// ONLY — build/deploy settings live in config.toml (internal/deployspec),
// replica counts and secrets in the control-plane DB.
package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"conductor/internal/target"
)

const DirName = ".conductor"

const fileName = "config.json"

const DefaultEnvironment = "production"

// ErrNotFound means no .conductor/config.json exists between cwd and the
// filesystem root. Callers that require a link turn this into an action hint.
var ErrNotFound = errors.New("no " + DirName + "/" + fileName + " found in this directory or any parent")

// Find walks up from cwd looking for DirName/fileName, returning the directory
// that contains the .conductor dir. ok=false means no link exists.
func Find() (dir string, ok bool, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	for {
		if _, statErr := os.Stat(Path(cwd)); statErr == nil {
			return cwd, true, nil
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return "", false, nil
		}
		cwd = parent
	}
}

func Load() (l target.Target, dir string, ok bool, err error) {
	dir, ok, err = Find()
	if err != nil || !ok {
		return target.Target{}, "", ok, err
	}
	path := Path(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		return target.Target{}, dir, true, err
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return target.Target{}, dir, true, fmt.Errorf("parse %s: %w", path, err)
	}
	return l, dir, true, nil
}

// Save writes l under dir/.conductor/config.json atomically (temp + rename) so
// a crash mid-write can't leave a half-serialized file.
func Save(dir string, l target.Target) error {
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	path := Path(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Remove() (dir string, err error) {
	dir, ok, err := Find()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotFound
	}
	if err := os.Remove(Path(dir)); err != nil {
		return "", err
	}
	// Tolerate leftovers (e.g. a stray .tmp): removing the link file is the
	// contract, not the dir.
	_ = os.Remove(filepath.Join(dir, DirName))
	return dir, nil
}

func SetEnvironment(env string) (dir string, err error) {
	return update(func(l *target.Target) { l.Environment = env })
}

func SetService(svc string) (dir string, err error) {
	return update(func(l *target.Target) { l.Service = svc })
}

func update(mutate func(*target.Target)) (dir string, err error) {
	l, dir, ok, err := Load()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotFound
	}
	mutate(&l)
	return dir, Save(dir, l)
}

func Path(dir string) string {
	return filepath.Join(dir, DirName, fileName)
}
