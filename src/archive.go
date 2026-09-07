package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const archivePageSize = 100
const archiveSchema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA cache_size=-8192;
CREATE TABLE IF NOT EXISTS entries (
 id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, at TEXT NOT NULL,
 kind TEXT NOT NULL, namespace TEXT NOT NULL, subject TEXT NOT NULL,
 operation TEXT NOT NULL, container TEXT NOT NULL, message TEXT NOT NULL, data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS entries_namespace ON entries(namespace,id DESC);
CREATE INDEX IF NOT EXISTS entries_subject ON entries(namespace,subject,id DESC);
CREATE INDEX IF NOT EXISTS entries_kind ON entries(kind,id DESC);
CREATE TABLE IF NOT EXISTS cursors (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE VIRTUAL TABLE IF NOT EXISTS entry_search USING fts5(message,content='entries',content_rowid='id');
CREATE TRIGGER IF NOT EXISTS entries_search AFTER INSERT ON entries BEGIN
 INSERT INTO entry_search(rowid,message) VALUES(new.id,new.message);
END;
PRAGMA user_version=1;
`

type archiveEntry struct {
	ID        int64  `json:"id,omitempty"`
	Key       string `json:"key"`
	At        string `json:"at"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Subject   string `json:"subject"`
	Operation string `json:"operation"`
	Container string `json:"container"`
	Message   string `json:"message"`
	Data      string `json:"data"`
	Source    string `json:"source"`
	Cursor    string `json:"cursor"`
}
type archiveFilter struct {
	Namespace, Subject, Kind, Search string
	Before                           int64
}
type archive struct {
	path   string
	queue  chan archiveEntry
	done   chan struct{}
	mu     sync.Mutex
	err    error
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
	stderr bytes.Buffer
	waited bool
}

func archiveDirectory() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "kiwikube")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "kiwikube")
}
func archivePath(dir, cluster string) string {
	sum := sha256.Sum256([]byte(cluster))
	return filepath.Join(dir, hex.EncodeToString(sum[:16])+".sqlite3")
}
func sqlText(s string) string    { return "CAST(X'" + hex.EncodeToString([]byte(s)) + "' AS TEXT)" }
func archiveKey(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func openArchive(path string) (*archive, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	a := &archive{path: path, queue: make(chan archiveEntry, 64), done: make(chan struct{})}
	a.cmd = exec.Command("sqlite3", "-batch", "-bail", path)
	// Let the parent drain commits on Ctrl+C; kill the child if the parent dies abruptly.
	a.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	a.cmd.Stderr = &a.stderr
	a.input, err = a.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pipe, err := a.cmd.StdoutPipe()
	if err != nil {
		a.input.Close()
		return nil, err
	}
	a.output = bufio.NewScanner(pipe)
	if err = a.cmd.Start(); err != nil {
		a.input.Close()
		pipe.Close()
		return nil, fmt.Errorf("archive requires sqlite3 with FTS5: %w", err)
	}
	if err = a.exec(".timeout 5000\n" + archiveSchema); err != nil {
		a.input.Close()
		return nil, err
	}
	go a.writeLoop()
	return a, nil
}

// A single persistent sqlite3 process owns batch writes. No subprocess per log line or commit.
func (a *archive) exec(sql string) error {
	if _, err := io.WriteString(a.input, sql+"\n.print KIWI_COMMITTED\n"); err != nil {
		return a.processError(err)
	}
	for a.output.Scan() {
		if a.output.Text() == "KIWI_COMMITTED" {
			return nil
		}
	}
	return a.processError(a.output.Err())
}
func (a *archive) processError(cause error) error {
	if !a.waited {
		if err := a.cmd.Wait(); cause == nil {
			cause = err
		}
		a.waited = true
	}
	return fmt.Errorf("archive write failed: %s", first(strings.TrimSpace(a.stderr.String()), fmt.Sprint(cause)))
}
func (a *archive) failure() error { a.mu.Lock(); defer a.mu.Unlock(); return a.err }
func (a *archive) add(ctx context.Context, e archiveEntry) error {
	if err := a.failure(); err != nil {
		return err
	}
	if len(e.Data)+len(e.Message) > 2*1024*1024 {
		return fmt.Errorf("archive record exceeds 2 MiB")
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	select {
	case a.queue <- e:
		return nil
	case <-a.done:
		return a.failure()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *archive) writeLoop() {
	defer close(a.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]archiveEntry, 0, 256)
	size := 0
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		err := a.writeBatch(batch)
		if err != nil {
			a.mu.Lock()
			a.err = err
			a.mu.Unlock()
			return false
		}
		clear(batch)
		batch = batch[:0]
		size = 0
		return true
	}
	for {
		select {
		case e, ok := <-a.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			size += len(e.Data) + len(e.Message) + 256
			if len(batch) >= 256 || size >= 1024*1024 {
				if !flush() {
					return
				}
			}
		case <-ticker.C:
			if !flush() {
				return
			}
		}
	}
}
func (a *archive) writeBatch(batch []archiveEntry) error {
	data, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	// One JSON input keeps SQL parsing bounded and gives inserts + replay checkpoints one atomic commit.
	return a.exec(`BEGIN IMMEDIATE;
CREATE TEMP TABLE IF NOT EXISTS batch(value TEXT);
DELETE FROM batch;
INSERT INTO batch SELECT value FROM json_each(` + sqlText(string(data)) + `);
INSERT INTO entries(key,at,kind,namespace,subject,operation,container,message,data)
 SELECT json_extract(value,'$.key'),json_extract(value,'$.at'),json_extract(value,'$.kind'),json_extract(value,'$.namespace'),json_extract(value,'$.subject'),json_extract(value,'$.operation'),json_extract(value,'$.container'),json_extract(value,'$.message'),json_extract(value,'$.data') FROM batch WHERE json_extract(value,'$.key')!=''
 ON CONFLICT(key) DO NOTHING;
INSERT INTO cursors(key,value)
 SELECT json_extract(value,'$.source'),json_extract(value,'$.cursor') FROM batch WHERE json_extract(value,'$.source')!=''
 ON CONFLICT(key) DO UPDATE SET value=CASE WHEN excluded.key LIKE 'log/%' THEN max(cursors.value,excluded.value) ELSE excluded.value END;
COMMIT;`)
}
func (a *archive) close() error {
	close(a.queue)
	<-a.done
	a.input.Close()
	if !a.waited {
		err := a.cmd.Wait()
		a.waited = true
		if err != nil && a.failure() == nil {
			return err
		}
	}
	return a.failure()
}
func archiveRead(ctx context.Context, path, sql string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", "-batch", "-bail", "-json", "-cmd", ".timeout 3000", "-cmd", ".explain off", path, sql)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("archive read: %s", first(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return data, nil
}
func readCursor(ctx context.Context, path, key string) (string, error) {
	data, err := archiveRead(ctx, path, "SELECT value FROM cursors WHERE key="+sqlText(key)+";")
	if err != nil {
		return "", err
	}
	var rows []struct{ Value string }
	if len(data) > 0 {
		if err = json.Unmarshal(data, &rows); err != nil {
			return "", err
		}
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].Value, nil
}
func archiveSQL(f archiveFilter, full bool) string {
	where := []string{"1=1"}
	from, order := "entries e", "e.id"
	if f.Namespace != "" {
		where = append(where, "e.namespace="+sqlText(f.Namespace))
	}
	if f.Subject != "" {
		where = append(where, "e.subject="+sqlText(f.Subject))
	}
	if f.Kind != "" {
		where = append(where, "e.kind="+sqlText(f.Kind))
	}
	if f.Before > 0 {
		where = append(where, "e.id<"+strconv.FormatInt(f.Before, 10))
	}
	if terms := strings.Fields(f.Search); len(terms) > 0 {
		for i, s := range terms {
			terms[i] = `"` + strings.ReplaceAll(s, `"`, `""`) + `"*`
		}
		from, order = "entry_search JOIN entries e ON e.id=entry_search.rowid", "entry_search.rowid"
		where = append(where, "entry_search MATCH "+sqlText(strings.Join(terms, " AND ")))
		if f.Before > 0 {
			where = append(where, "entry_search.rowid<"+strconv.FormatInt(f.Before, 10))
		}
	}
	fields := "e.id,e.at,e.kind,e.namespace,e.subject,e.operation,e.container,substr(e.message,1,4096) AS message"
	if full {
		fields = "e.*"
	}
	return "SELECT " + fields + " FROM " + from + " WHERE " + strings.Join(where, " AND ") + " ORDER BY " + order + " DESC LIMIT " + strconv.Itoa(archivePageSize) + ";"
}
func readArchive(ctx context.Context, path string, f archiveFilter) ([]archiveEntry, error) {
	data, err := archiveRead(ctx, path, archiveSQL(f, false))
	if err != nil {
		return nil, err
	}
	var rows []archiveEntry
	if len(data) > 0 {
		err = json.Unmarshal(data, &rows)
	}
	return rows, err
}
func archiveDetail(ctx context.Context, path string, id int64) ([]string, error) {
	data, err := archiveRead(ctx, path, "SELECT * FROM entries WHERE id="+strconv.FormatInt(id, 10)+";")
	if err != nil {
		return nil, err
	}
	var rows []archiveEntry
	if err = json.Unmarshal(data, &rows); err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("archive entry unavailable: %v", err)
	}
	e := rows[0]
	body := e.Data
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(body), "", "  ") == nil {
		body = pretty.String()
	}
	return append([]string{e.At + " · " + e.Operation + " · " + e.Subject, e.Message, ""}, strings.Split(body, "\n")...), nil
}
