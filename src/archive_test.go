package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchivePersistenceReplayAndPaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.sqlite3")
	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 250; i++ {
		e := archiveEntry{Key: fmt.Sprint(i), Kind: "logs", Namespace: "production", Subject: "Pod/api", Message: fmt.Sprintf("request accepted %d O'Brien", i), Data: `{"message":"exact\u001b[31m data"}`, Source: "log/pod/api/0", Cursor: fmt.Sprintf("%019d", i)}
		if err = a.add(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
	a, err = openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.add(ctx, archiveEntry{Key: "249", Kind: "logs", Namespace: "production", Subject: "Pod/api", Message: "duplicate", Source: "log/pod/api/0", Cursor: "0000000000000000001"}); err != nil {
		t.Fatal(err)
	}
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
	cursor, err := readCursor(ctx, path, "log/pod/api/0")
	if err != nil || cursor != "0000000000000000249" {
		t.Fatalf("durable cursor regressed: %q %v", cursor, err)
	}
	filter := archiveFilter{Namespace: "production", Subject: "Pod/api", Search: "accepted"}
	total := 0
	last := int64(1 << 62)
	for {
		rows, err := readArchive(ctx, path, filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID >= last {
				t.Fatal("duplicate or unordered page")
			}
			last = r.ID
			total++
		}
		if len(rows) < archivePageSize {
			break
		}
		filter.Before = last
	}
	if total != 250 {
		t.Fatalf("got %d records after restart and replay", total)
	}
	rows, err := readArchive(ctx, path, archiveFilter{Search: "O'Brien"})
	if err != nil || len(rows) != 100 {
		t.Fatalf("quoted search: %d %v", len(rows), err)
	}
	rows, err = readArchive(ctx, path, archiveFilter{Namespace: "production'; DROP TABLE entries;--"})
	if err != nil || len(rows) != 0 {
		t.Fatal("unsafe SQL filter")
	}
	lines, err := archiveDetail(ctx, path, last)
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), `\u001b`) {
		t.Fatalf("full original payload not retained: %v %v", lines, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("archive must be private")
	}
}
func TestArchiveFailureIsVisibleAndCheckpointDoesNotAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.sqlite3")
	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = exec.Command("sqlite3", path, "DROP TABLE entries;").Run(); err != nil {
		t.Fatal(err)
	}
	_ = a.add(context.Background(), archiveEntry{Key: "record", Message: "must not commit", Source: "watch/pods", Cursor: "99"})
	if err = a.close(); err == nil {
		t.Fatal("write failure hidden")
	}
	cursor, err := readCursor(context.Background(), path, "watch/pods")
	if err != nil || cursor != "" {
		t.Fatalf("failed transaction committed checkpoint: %q %v", cursor, err)
	}
}
func TestArchiveModelPagingAndSearch(t *testing.T) {
	a := newApp()
	a.view = &viewer{kind: "archive", entries: make([]archiveEntry, archivePageSize)}
	a.view.entries[99].ID = 123
	a.handle(key{r: 'n'})
	if a.action != "reload" || a.view.archiveFilter.Before != 123 {
		t.Fatal("older page")
	}
	a.action = ""
	a.handle(key{r: 'N'})
	if a.view.archiveFilter.Before != 0 {
		t.Fatal("newer page")
	}
	a.handle(key{r: '/'})
	for _, r := range "error api" {
		a.handle(key{r: r})
	}
	a.action = ""
	a.handle(key{code: keyEnter})
	if a.action != "reload" || a.view.archiveFilter.Search != "error api" {
		t.Fatal("archive search must query the database")
	}
}
func BenchmarkArchiveBatch(b *testing.B) {
	a, err := openArchive(filepath.Join(b.TempDir(), "bench.sqlite3"))
	if err != nil {
		b.Fatal(err)
	}
	defer a.close()
	batch := make([]archiveEntry, 256)
	message := strings.Repeat("request served successfully ", 8)
	for i := range batch {
		batch[i] = archiveEntry{At: time.Now().UTC().Format(time.RFC3339Nano), Kind: "logs", Namespace: "production", Subject: "Pod/api", Operation: "log", Message: message, Data: `{"message":"` + message + `"}`}
	}
	b.SetBytes(int64(len(message) * len(batch)))
	b.ResetTimer()
	n := 0
	for b.Loop() {
		for i := range batch {
			batch[i].Key = fmt.Sprintf("%d-%d", n, i)
		}
		n++
		if err = a.writeBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
}
func TestArchiveQueryUsesIndexes(t *testing.T) {
	a, err := openArchive(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	for _, f := range []archiveFilter{{Namespace: "production", Subject: "Pod/api", Before: 500}, {Search: "request", Before: 500}} {
		data, err := archiveRead(context.Background(), a.path, "EXPLAIN QUERY PLAN "+archiveSQL(f, false))
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct{ Detail string }
		if err = json.Unmarshal(data, &rows); err != nil {
			t.Fatal(err)
		}
		plan := string(data)
		if strings.Contains(plan, "SCAN e ") || strings.Contains(plan, "LIST SUBQUERY") || strings.Contains(plan, "USE TEMP B-TREE") {
			t.Fatalf("history paging must not scan/sort full archive: %s", plan)
		}
	}
}

func BenchmarkArchiveQuery100K(b *testing.B) {
	a, err := openArchive(filepath.Join(b.TempDir(), "query.sqlite3"))
	if err != nil {
		b.Fatal(err)
	}
	defer a.close()
	batch := make([]archiveEntry, 256)
	for base := 0; base < 100000; base += len(batch) {
		for i := range batch {
			batch[i] = archiveEntry{Key: fmt.Sprint(base + i), Kind: "logs", Namespace: "production", Subject: "Pod/api", Message: "request accepted", Data: `{"message":"request accepted"}`}
		}
		if err = a.writeBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
	for _, test := range []struct {
		name   string
		filter archiveFilter
	}{
		{"recent", archiveFilter{}},
		{"resource", archiveFilter{Namespace: "production", Subject: "Pod/api", Before: 50000}},
		{"common_word", archiveFilter{Search: "request", Before: 50000}},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := readArchive(context.Background(), a.path, test.filter)
				if err != nil || len(rows) != archivePageSize {
					b.Fatalf("query: %d %v", len(rows), err)
				}
			}
		})
	}
}
