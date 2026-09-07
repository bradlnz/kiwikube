package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type collector struct {
	api      *kubeAPI
	archive  *archive
	ctx      context.Context
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	lock     *os.File
	slots    chan struct{}
	active   atomic.Int32
	queued   atomic.Int32
	tasks    map[string]context.CancelFunc // Owned by the pod discovery watch.
	problems sync.Map
}

func startCollector(ctx context.Context, api *kubeAPI, a *archive, streams int) (*collector, error) {
	lock, err := os.OpenFile(a.path+".collector.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &collector{api: api, archive: a, ctx: ctx, cancel: cancel, lock: lock, slots: make(chan struct{}, streams), tasks: map[string]context.CancelFunc{}}
	for _, kind := range append([]resourceType{{key: "namespaces", kind: "Namespace"}}, resourceTypes...) {
		if kind.key == "secrets" {
			continue // Secret values are fetched for display, never recorded to the archive.
		}
		c.workers.Go(func() {
			c.watch(kind, true, func(operation string, raw json.RawMessage) error { return c.storeResource(kind, operation, raw) })
		})
	}
	// A current pod watch discovers log sources independently of historical replay.
	c.workers.Go(func() { c.watch(resourceType{key: "pods", kind: "Pod"}, false, c.podChanged) })
	return c, nil
}
func (c *collector) close() {
	if c == nil {
		return
	}
	c.cancel()
	c.workers.Wait()
	_ = c.lock.Close()
}
func (c *collector) status() string {
	if err := c.archive.failure(); err != nil {
		return "ARCHIVE FAILED · " + err.Error()
	}
	message := fmt.Sprintf("Archive · %d active / %d queued logs", c.active.Load(), c.queued.Load())
	count := 0
	firstError := ""
	c.problems.Range(func(_, v any) bool {
		count++
		if firstError == "" || v.(string) < firstError {
			firstError = v.(string)
		}
		return true
	})
	if count > 0 {
		message += fmt.Sprintf(" · %d capture issues: %s", count, firstError)
	}
	return message
}
func (c *collector) problem(source string, err error) {
	if err == nil {
		c.problems.Delete(source)
		return
	}
	if c.ctx.Err() != nil {
		return
	}
	message := source + ": " + err.Error()
	if old, ok := c.problems.Load(source); ok && old == message {
		return
	}
	c.problems.Store(source, message)
	_ = c.archive.add(c.ctx, archiveEntry{Key: archiveKey(time.Now().String() + message), Kind: "collector", Subject: source, Operation: "capture-error", Message: message})
}
func pauseContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (c *collector) watch(kind resourceType, persist bool, accept func(string, json.RawMessage) error) {
	source := "watch/" + kind.key
	if !persist {
		source = "discovery/pods"
	}
	version := ""
	if persist {
		for {
			var err error
			version, err = readCursor(c.ctx, c.archive.path, source)
			if err == nil {
				break
			}
			c.problem(source, err)
			if !pauseContext(c.ctx, time.Second) {
				return
			}
		}
	}
	delay := time.Second
	for c.ctx.Err() == nil && c.archive.failure() == nil {
		if version == "" {
			if !persist {
				_ = accept("RESET", nil)
			}
			listCtx, stop := context.WithTimeout(c.ctx, 30*time.Second)
			rv, err := c.api.list(listCtx, kind.key, func(raw json.RawMessage) error { return accept("OBSERVED", raw) })
			stop()
			if err != nil {
				c.problem(source, err)
				if !pauseContext(c.ctx, delay) {
					return
				}
				delay = min(delay*2, 30*time.Second)
				continue
			}
			version = rv
			if persist {
				if err = c.archive.add(c.ctx, archiveEntry{Source: source, Cursor: version}); err != nil {
					return
				}
			}
		}
		query := url.Values{"watch": {"true"}, "resourceVersion": {version}, "allowWatchBookmarks": {"true"}, "timeoutSeconds": {"300"}}
		request, stop := context.WithTimeout(c.ctx, 320*time.Second)
		body, err := c.api.get(request, resourcePath(kind.key), query)
		if err == nil {
			c.problem(source, nil)
			delay = time.Second
			decoder := json.NewDecoder(body)
			for {
				var event struct {
					Type   string
					Object json.RawMessage
				}
				if err = decoder.Decode(&event); err != nil {
					break
				}
				if event.Type == "ERROR" {
					var status struct {
						Code    int
						Message string
					}
					_ = json.Unmarshal(event.Object, &status)
					err = &apiError{status.Code, status.Message}
					break
				}
				var object struct{ Metadata metadata }
				if err = json.Unmarshal(event.Object, &object); err != nil {
					break
				}
				if event.Type != "BOOKMARK" {
					if err = accept(event.Type, event.Object); err != nil {
						break
					}
				}
				version = first(object.Metadata.ResourceVersion, version)
				if persist && event.Type == "BOOKMARK" {
					if err = c.archive.add(c.ctx, archiveEntry{Source: source, Cursor: version}); err != nil {
						break
					}
				}
			}
			body.Close()
		}
		stop()
		if c.ctx.Err() != nil {
			return
		}
		var status *apiError
		if errors.As(err, &status) && status.code == 410 {
			c.problem(source, fmt.Errorf("watch history expired; resyncing current state (gap in upstream history)"))
			version = ""
		} else if err != nil && err != io.EOF {
			c.problem(source, err)
		}
		if !pauseContext(c.ctx, delay) {
			return
		}
		delay = min(delay*2, 30*time.Second)
	}
}
func (c *collector) storeResource(kind resourceType, operation string, raw json.RawMessage) error {
	var o object
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	o.Kind = kind.kind
	r := o.record()
	subject := kind.kind + "/" + r.Name
	if kind.key == "events" {
		subject = o.InvolvedObject.Kind + "/" + o.InvolvedObject.Name
	}
	identity := kind.key + "/" + r.id() + "/" + r.ResourceVersion
	if r.ResourceVersion == "" {
		identity += "/" + archiveKey(string(raw))
	}
	if operation == "DELETED" {
		identity += "/deleted"
	}
	source := "watch/" + kind.key
	if operation == "OBSERVED" {
		source = ""
	}
	return c.archive.add(c.ctx, archiveEntry{Key: archiveKey(identity), Kind: kind.key, Namespace: r.Namespace, Subject: subject, Operation: operation, Message: strings.Join(r.Cells, " · "), Data: string(raw), Source: source, Cursor: r.ResourceVersion})
}

type logPod struct {
	Metadata metadata
	Spec     struct{ Containers, InitContainers, EphemeralContainers []containerSpec }
	Status   struct{ ContainerStatuses, InitContainerStatuses, EphemeralContainerStatuses []logContainer }
}
type logContainer struct {
	Name         string
	RestartCount int
	State        struct{ Running, Terminated json.RawMessage }
}

func (c *collector) podChanged(operation string, raw json.RawMessage) error {
	if operation == "RESET" {
		for _, cancel := range c.tasks {
			cancel()
		}
		c.tasks = map[string]context.CancelFunc{}
		return nil
	}
	var pod logPod
	if err := json.Unmarshal(raw, &pod); err != nil {
		return err
	}
	prefix := first(pod.Metadata.UID, pod.Metadata.Namespace+"/"+pod.Metadata.Name) + "/"
	desired := map[string]bool{}
	if operation != "DELETED" {
		statuses := append(append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...), pod.Status.EphemeralContainerStatuses...)
		for _, container := range statuses {
			for _, previous := range []bool{false, true} {
				if previous && container.RestartCount == 0 {
					continue
				}
				if !previous && len(container.State.Running) == 0 && len(container.State.Terminated) == 0 {
					continue
				}
				restart := container.RestartCount
				if previous {
					restart--
				}
				source := "log/" + prefix + container.Name + "/" + strconv.Itoa(restart)
				task := prefix + container.Name + "/" + strconv.Itoa(restart) + "/" + strconv.FormatBool(previous)
				desired[task] = true
				if _, exists := c.tasks[task]; exists {
					continue
				}
				ctx, cancel := context.WithCancel(c.ctx)
				c.tasks[task] = cancel
				c.workers.Go(func() {
					c.captureLogs(ctx, pod.Metadata, container.Name, source, previous, len(container.State.Terminated) > 0)
				})
			}
		}
	}
	for key, cancel := range c.tasks {
		if strings.HasPrefix(key, prefix) && !desired[key] {
			cancel()
			delete(c.tasks, key)
			c.problems.Delete(key)
		}
	}
	return nil
}
func (c *collector) captureLogs(ctx context.Context, pod metadata, container, source string, previous, terminated bool) {
	defer c.problems.Delete(source)
	delay := time.Second
	for ctx.Err() == nil && c.archive.failure() == nil {
		c.queued.Add(1)
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			c.queued.Add(-1)
			return
		}
		c.queued.Add(-1)
		c.active.Add(1)
		err := c.readLogs(ctx, pod, container, source, previous, terminated)
		c.active.Add(-1)
		<-c.slots
		if ctx.Err() != nil {
			return
		}
		c.problem(source, err)
		var unavailable *apiError
		if previous && errors.As(err, &unavailable) && unavailable.code == 400 && strings.Contains(unavailable.message, "not found") {
			return
		} // Already-rotated previous logs cannot appear later.
		if err == nil && (previous || terminated) {
			return
		}
		if !pauseContext(ctx, delay) {
			return
		}
		delay = min(delay*2, 30*time.Second)
	}
}
func (c *collector) readLogs(ctx context.Context, pod metadata, container, source string, previous, terminated bool) error {
	// Rotate one-minute requests so queued containers get a turn without an unbounded connection count.
	request, stop := context.WithTimeout(ctx, time.Minute)
	defer stop()
	path := "/api/v1/namespaces/" + url.PathEscape(pod.Namespace) + "/pods/" + url.PathEscape(pod.Name)
	identity, err := c.api.get(request, path, nil)
	if err != nil {
		return err
	}
	var current struct{ Metadata metadata }
	err = json.NewDecoder(identity).Decode(&current)
	identity.Close()
	if err != nil {
		return err
	}
	if pod.UID != "" && current.Metadata.UID != pod.UID {
		return fmt.Errorf("pod UID changed before log request")
	}
	cursor, err := readCursor(request, c.archive.path, source)
	if err != nil {
		return err
	}
	query := url.Values{"container": {container}, "timestamps": {"true"}, "follow": {strconv.FormatBool(!previous && !terminated)}, "previous": {strconv.FormatBool(previous)}}
	if stamp, err := strconv.ParseInt(cursor, 10, 64); err == nil {
		query.Set("sinceTime", time.Unix(0, stamp).Add(-time.Second).UTC().Format(time.RFC3339Nano))
	}
	body, err := c.api.get(request, path+"/log", query)
	if err != nil {
		return err
	}
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	lastStamp := ""
	counts := map[string]int{}
	for scanner.Scan() {
		line := scanner.Text()
		stamp, message, ok := strings.Cut(line, " ")
		when, parseErr := time.Parse(time.RFC3339Nano, stamp)
		if !ok || parseErr != nil {
			return fmt.Errorf("log source did not provide a valid timestamp")
		}
		if stamp != lastStamp {
			clear(counts)
			lastStamp = stamp
		}
		hash := archiveKey(message)
		ordinal := counts[hash]
		counts[hash]++
		key := archiveKey(source + "/" + stamp + "/" + hash + "/" + strconv.Itoa(ordinal))
		data, _ := json.Marshal(map[string]string{"timestamp": stamp, "message": message})
		e := archiveEntry{Key: key, At: when.UTC().Format(time.RFC3339Nano), Kind: "logs", Namespace: pod.Namespace, Subject: "Pod/" + pod.Name, Container: container, Operation: "log", Message: message, Data: string(data), Source: source, Cursor: strconv.FormatInt(when.UnixNano(), 10)}
		if err = c.archive.add(ctx, e); err != nil {
			return err
		}
	}
	if request.Err() != nil && ctx.Err() == nil {
		return nil
	}
	if err = scanner.Err(); err != nil {
		return fmt.Errorf("log stream (maximum line 1 MiB): %w", err)
	}
	return nil
}
