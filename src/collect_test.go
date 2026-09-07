package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectorWatchRecoveryLogsAndRestart(t *testing.T) {
	var expired atomic.Bool
	var resumed atomic.Bool
	var logRequests atomic.Int32
	pod := `{"metadata":{"name":"api","namespace":"default","uid":"pod-uid","resourceVersion":"10"},"spec":{"containers":[{"name":"api"}]},"status":{"containerStatuses":[{"name":"api","restartCount":1,"state":{"running":{}}}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path == "/api/v1/namespaces/default/pods/api" {
			fmt.Fprint(w, pod)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			logRequests.Add(1)
			if q.Get("previous") == "true" {
				fmt.Fprint(w, "2026-09-07T00:00:00Z previous instance\n")
				return
			}
			fmt.Fprint(w, "2026-09-07T00:00:01Z same line\n2026-09-07T00:00:01Z same line\n2026-09-07T00:00:02Z last line\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		deployment := strings.HasSuffix(r.URL.Path, "/deployments")
		if q.Get("watch") == "true" {
			rv := q.Get("resourceVersion")
			if deployment && rv == "7" && !expired.Swap(true) {
				http.Error(w, "history expired", 410)
				return
			}
			if deployment && rv == "10" {
				for i, operation := range []string{"MODIFIED", "DELETED"} {
					fmt.Fprintf(w, `{"type":%q,"object":{"metadata":{"name":"api","namespace":"default","uid":"deployment-uid","resourceVersion":%q},"spec":{"replicas":2}}}`+"\n", operation, fmt.Sprint(11+i))
				}
			}
			if deployment && rv == "12" {
				resumed.Store(true)
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		items := "[]"
		if deployment {
			items = `[{"metadata":{"name":"api","namespace":"default","uid":"deployment-uid","resourceVersion":"10"},"spec":{"replicas":1}}]`
		}
		if r.URL.Path == "/api/v1/pods" {
			items = "[" + pod + "]"
		}
		fmt.Fprintf(w, `{"metadata":{"resourceVersion":"10"},"items":%s}`, items)
	}))
	defer server.Close()
	api := &kubeAPI{client: server.Client(), base: server.URL}
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.add(context.Background(), archiveEntry{Source: "watch/deployments", Cursor: "7"})
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
	run := func() {
		before := logRequests.Load()
		a, err = openArchive(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := startCollector(context.Background(), api, a, 4)
		if err != nil {
			a.close()
			t.Fatal(err)
		}
		defer func() {
			c.close()
			if err := a.close(); err != nil {
				t.Error(err)
			}
		}()
		other, err := startCollector(context.Background(), api, a, 4)
		if err != nil || other != nil {
			if other != nil {
				other.close()
			}
			t.Fatal("duplicate collector lock was not respected")
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			rows, _ := readArchive(context.Background(), path, archiveFilter{Kind: "logs"})
			rv, _ := readCursor(context.Background(), path, "watch/deployments")
			if len(rows) == 4 && rv == "12" && logRequests.Load() >= before+2 {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("watch/log collection did not complete", c.status())
	}
	run()
	run()
	if !expired.Load() {
		t.Fatal("expired watch was not resumed")
	}
	// Wait for the resumed watch request itself, not just its previously committed checkpoint.
	if !resumed.Load() {
		t.Fatal("second run did not resume the committed watch resourceVersion")
	}
	logs, err := readArchive(context.Background(), path, archiveFilter{Kind: "logs"})
	if err != nil || len(logs) != 4 {
		t.Fatalf("replay lost repeated lines or introduced duplicates: %+v %v", logs, err)
	}
	resources, err := readArchive(context.Background(), path, archiveFilter{Kind: "deployments"})
	if err != nil || len(resources) != 3 || resources[0].Operation != "DELETED" {
		t.Fatalf("deployment revisions/deletion not durable: %+v %v", resources, err)
	}
	diagnostics, _ := readArchive(context.Background(), path, archiveFilter{Kind: "collector"})
	if len(diagnostics) == 0 {
		t.Fatal("upstream history gap was not recorded")
	}
}
func TestAPIPaginationAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pods" {
			http.Error(w, "Forbidden", 403)
			return
		}
		if r.URL.Query().Get("continue") == "" {
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"42","continue":"next"},"items":[{"metadata":{"name":"a"}}]}`)
		} else {
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"42"},"items":[{"metadata":{"name":"b"}}]}`)
		}
	}))
	defer server.Close()
	api := &kubeAPI{client: server.Client(), base: server.URL}
	count := 0
	rv, err := api.list(context.Background(), "pods", func(json.RawMessage) error { count++; return nil })
	if err != nil || count != 2 || rv != "42" {
		t.Fatalf("paginated list: %d %q %v", count, rv, err)
	}
	if _, err = api.list(context.Background(), "nodes", func(json.RawMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("permission error lost: %v", err)
	}
}
