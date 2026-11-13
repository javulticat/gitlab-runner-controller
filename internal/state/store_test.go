package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), "test", "gitlab/1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestTransactionFailurePoisonsWithoutChangingMemory(t *testing.T) {
	s := openTest(t)
	s.write = func(string, State) error { return errors.New("disk full") }
	err := s.Update(func(value *State) error { value.HintRevision++; return nil })
	if !errors.Is(err, ErrPoisoned) || s.Snapshot().HintRevision != 0 || s.Healthy() {
		t.Fatalf("failed transaction advanced state: %v", err)
	}
	if err := s.Update(func(*State) error { return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal("poisoned store accepted a transaction")
	}
}

func TestExclusiveLockRestartAndPermissions(t *testing.T) {
	s := openTest(t)
	if other, err := Open(s.path, "test", "gitlab/1"); err == nil {
		_ = other.Close()
		t.Fatal("second writer acquired the lock")
	}
	if err := s.Update(func(value *State) error { value.HintRevision++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path, "test", "gitlab/1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if reopened.Snapshot().HintRevision != 1 {
		t.Fatal("state was not durable")
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
}

func TestCorruptLegacyAndForeignStateFailClosed(t *testing.T) {
	for _, content := range []string{`{"legacy_id":"legacy"}`, `{"version":2}`, `null`, `{} {}`, `{}`, `{"version":1}`, `{"version":1,"controller_id":"test","binding":"gitlab/1","jobs":{},"hints":{},"provider_backoff":{}}`} {
		t.Run(content, func(t *testing.T) {
			assertRejectedStateFile(t, []byte(content))
		})
	}
	s := openTest(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(s.path, "other", "gitlab/1"); err == nil {
		_ = other.Close()
		t.Fatal("foreign state accepted")
	}
}

func assertRejectedStateFile(t *testing.T, content []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func(string, string, string) (*Store, error){Open, OpenForRecovery} {
		if s, err := open(path, "test", "gitlab/1"); err == nil {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
			t.Fatal("invalid state accepted")
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, content) {
			t.Fatal("invalid state was overwritten")
		}
	}
}

func TestUnsupportedSnapshotVersionsAreRejectedWithoutRewriting(t *testing.T) {
	for _, version := range []int{-1, 0, Version + 1, Version + 10} {
		saved := New("test", "gitlab/1")
		saved.Version = version
		data, err := json.Marshal(saved)
		if err != nil {
			t.Fatal(err)
		}
		assertRejectedStateFile(t, data)
	}
}

func TestRecoveryOpenPreservesSnapshotAndExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if store, err := OpenForRecovery(path, "test", "gitlab/1"); err == nil {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("recovery created a missing snapshot")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery changed the missing snapshot: %v", err)
	}
	data, err := json.Marshal(New("test", "gitlab/1"))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n', '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenForRecovery(path, "test", "gitlab/1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, data) {
		t.Fatal("recovery open rewrote the snapshot before audit")
	}
	for _, open := range []func(string, string, string) (*Store, error){Open, OpenForRecovery} {
		if other, err := open(path, "test", "gitlab/1"); err == nil {
			if err := other.Close(); err != nil {
				t.Error(err)
			}
			t.Fatal("a second writer acquired the state lock during recovery")
		}
	}
}

func TestSnapshotAndRejectedTransactionAreIsolated(t *testing.T) {
	s := openTest(t)
	if err := s.Update(func(value *State) error {
		value.Pools["linux"] = Pool{Provider: "test", OptionsHash: "test", RunnerID: 1, Slots: []Slot{{Power: "stopped"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := s.Snapshot()
	p := snapshot.Pools["linux"]
	p.Slots[0].ResourceID = "changed"
	err := s.Update(func(value *State) error {
		p := value.Pools["linux"]
		p.Slots[0].ResourceID = "also-changed"
		return ErrStale
	})
	if !errors.Is(err, ErrStale) || s.Snapshot().Pools["linux"].Slots[0].ResourceID != "" {
		t.Fatal("snapshot or rejected transaction changed state")
	}
}

func TestOversizedWriteRetainsReopenableSnapshot(t *testing.T) {
	s := openTest(t)
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(saved *State) error {
		saved.Pools["large"] = Pool{Provider: "test", OptionsHash: "test", RunnerID: 1, Slots: []Slot{{Power: "stopped", LastError: strings.Repeat("x", MaxBytes)}}}
		return nil
	})
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("oversized state was committed: %v", err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil || string(before) != string(after) {
		t.Fatal("oversized write replaced the previous snapshot")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path, "test", "gitlab/1")
	if err != nil {
		t.Fatal("writer produced a snapshot that cannot reopen:", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedTransactionsDoNotWriteOrAdvanceRevision(t *testing.T) {
	s := openTest(t)
	if err := s.Update(func(saved *State) error {
		saved.Jobs["1/1"] = Job{ProjectID: 1, ID: 1, Status: "running", RunnerID: 7, Tags: []string{"linux"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	writes := 0
	write := s.write
	s.write = func(path string, saved State) error {
		writes++
		return write(path, saved)
	}
	for _, change := range []func(*State) error{
		func(*State) error { return nil },
		func(saved *State) error {
			job := saved.Jobs["1/1"]
			job.Tags = append([]string(nil), job.Tags...)
			saved.Jobs["1/1"] = job
			return nil
		},
		func(saved *State) error { saved.HintRevision++; saved.HintRevision--; return nil },
		func(*State) error { return ErrNoChange },
	} {
		if err := s.Update(change); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 0 || s.Snapshot().Revision != before.Revision {
		t.Fatalf("unchanged transactions caused %d writes and revision %d -> %d", writes, before.Revision, s.Snapshot().Revision)
	}
	if err := s.Update(func(saved *State) error { saved.HintRevision++; return nil }); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || s.Snapshot().Revision != before.Revision+1 {
		t.Fatal("changed transaction did not produce exactly one durable revision")
	}
}
