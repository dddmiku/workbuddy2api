// ═══ 更新日志 ═══
// 2026-09-18：复现并发落盘、快照别名、关闭时机与大账本/损坏账本的数据完整性问题。
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func readIntegrityDocument(t *testing.T, path string) document {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestPersistenceSnapshotDoesNotAliasLaterRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Record("key", "client", "masked", "model", 10, 0, 0, 0, false, time.Now())
	original := s.persist
	var first document
	s.persist = func(doc document) error {
		s.Record("key", "client", "masked", "model", 20, 0, 0, 0, false, time.Now())
		first = cloneDocument(doc)
		return original(doc)
	}
	err = s.Flush()
	s.persist = original
	if err != nil {
		t.Fatal(err)
	}
	if first.Totals.Requests != 1 || first.Keys["key"].Totals.Requests != 1 {
		t.Fatalf("flush snapshot changed after Record: total=%d key=%d", first.Totals.Requests, first.Keys["key"].Totals.Requests)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	doc := readIntegrityDocument(t, path)
	if doc.Totals.Requests != 2 || doc.Keys["key"].Totals.Requests != 2 || doc.Totals.TotalTokens != 30 {
		t.Fatalf("record during Flush lost or duplicated: %+v", doc.Totals)
	}
}

func TestPersistenceConcurrentStoresDoNotLoseCommittedRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	a, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	start := make(chan struct{})
	errors := make(chan error, 2)
	for index, store := range []*Store{a, b} {
		go func(index int, store *Store) {
			<-start
			for round := 0; round < 40; round++ {
				store.Record(fmt.Sprint(index), "client", "masked", "model", 1, 0, 0, 0, false, time.Now())
				if err := store.Flush(); err != nil {
					errors <- err
					return
				}
			}
			errors <- nil
		}(index, store)
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Error(err)
		}
	}
	doc := readIntegrityDocument(t, path)
	if doc.Totals.Requests != 80 {
		t.Fatalf("concurrent stores persisted %d requests, want 80", doc.Totals.Requests)
	}
}

func TestPersistenceCorruptionDoesNotEraseHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Record("key", "client", "masked", "model", 10, 0, 0, 0, false, time.Now())
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Record("key", "client", "masked", "model", 20, 0, 0, 0, false, time.Now())
	broken := []byte("{incomplete external write")
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err == nil {
		t.Error("corrupt ledger was silently overwritten")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(broken) {
		t.Error("failure overwrote the original corrupt evidence")
	}
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readIntegrityDocument(t, path).Totals.TotalTokens; got != 30 {
		t.Fatalf("repaired ledger lost pending usage: %d", got)
	}
}

func TestPersistenceLargeValidLedgerReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for key := 0; key < 60; key++ {
		for day := 0; day < 120; day++ {
			s.Record(fmt.Sprintf("key-%d", key), "client", "masked", "model", 10, 1, 0, 0, false, start.AddDate(0, 0, day))
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Size() <= 1<<20 {
		t.Fatal("fixture did not reach the legacy size limit")
	}
	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Snapshot().Totals.Requests; got != 7200 {
		t.Fatalf("requests=%d", got)
	}
}

func TestPersistenceConcurrentCloseWaitsForFinalFlush(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s.Record("key", "client", "masked", "model", 1, 0, 0, 0, false, time.Now())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	original := s.persist
	s.persist = func(doc document) error { close(entered); <-release; return original(doc) }
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- s.Close() }()
	<-entered
	go func() { second <- s.Close() }()
	select {
	case err := <-second:
		t.Errorf("second Close returned before durable flush: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}
