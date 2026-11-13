package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/gofrs/flock"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
)

var ErrPoisoned = errors.New("state persistence failed; restart after repairing storage")

const MaxBytes = 32 << 20

type Store struct {
	mu       sync.RWMutex
	path     string
	lock     *flock.Flock
	state    State
	poisoned bool
	closed   bool
	write    func(string, State) error
}

func Open(path, controllerID, binding string) (_ *Store, err error) {
	return open(path, controllerID, binding, false)
}

func OpenForRecovery(path, controllerID, binding string) (*Store, error) {
	return open(path, controllerID, binding, true)
}

func open(path, controllerID, binding string, recovery bool) (_ *Store, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect state directory: %w", err)
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock state: %w", err)
	}
	if !locked {
		return nil, errors.New("another controller owns this state file")
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Close())
		}
	}()
	file, err := os.Open(path)
	saved := State{}
	if err == nil {
		defer func() { err = errors.Join(err, file.Close()) }()
		data, readErr := io.ReadAll(io.LimitReader(file, MaxBytes+1))
		if readErr != nil {
			return nil, fmt.Errorf("read state: %w", readErr)
		}
		if len(data) > MaxBytes {
			return nil, errors.New("state exceeds 32 MiB")
		}
		if err := config.DecodeStrict(data, &saved); err != nil {
			return nil, fmt.Errorf("decode state; preserve the file for recovery: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open state: %w", err)
	} else {
		if recovery {
			return nil, errors.New("recovery requires an existing state file")
		}
		saved = New(controllerID, binding)
	}
	if err := saved.Validate(); err != nil {
		return nil, err
	}
	if saved.ControllerID != controllerID || saved.Binding != binding {
		return nil, errors.New("state belongs to a different controller, GitLab URL, or project set")
	}
	store := &Store{path: path, lock: lock, state: saved, write: atomicWrite}
	if !recovery {
		if err := store.write(path, saved); err != nil {
			return nil, fmt.Errorf("verify state storage: %w", err)
		}
	}
	return store, nil
}

func (s *Store) Backup() (name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return "", errors.New("state store must be healthy and locked before backup")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".backup-*")
	if err != nil {
		return "", err
	}
	name = file.Name()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return name, err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return name, err
	}
	return name, errors.Join(dir.Sync(), dir.Close())
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Clone()
}

func (s *Store) Healthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.poisoned && !s.closed
}

func (s *Store) Update(change func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned || s.closed {
		return ErrPoisoned
	}
	next := s.state.Clone()
	if err := change(&next); err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		return err
	}
	if reflect.DeepEqual(next, s.state) {
		return nil
	}
	next.Revision++
	if err := next.Validate(); err != nil {
		return fmt.Errorf("invalid state transaction: %w", err)
	}
	if err := s.write(s.path, next); err != nil {
		s.poisoned = true
		return fmt.Errorf("%w: %w", ErrPoisoned, err)
	}
	s.state = next
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

func atomicWrite(path string, value State) (err error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > MaxBytes {
		return errors.New("state exceeds 32 MiB; previous snapshot retained")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		if removeErr := os.Remove(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
