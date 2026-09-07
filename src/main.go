package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kiwikube:", err)
		os.Exit(1)
	}
}

type clusterSwitch struct {
	name     string
	settings config
}

func (s *clusterSwitch) Error() string { return "switch cluster to " + s.name }
func run() error {
	term := &terminal{}
	defer term.close()
	var selected *clusterSwitch
	for {
		err := runSession(selected, term)
		next, ok := err.(*clusterSwitch)
		if !ok {
			return err
		}
		selected = next
	}
}
func runSession(selected *clusterSwitch, term *terminal) (resultErr error) {
	flags := flag.NewFlagSet("kiwikube", flag.ContinueOnError)
	path := flags.String("config", configPath(), "settings file")
	theme := flags.String("theme", "", "kiwi, ocean, or amber")
	interval := flags.Int("refresh", 0, "refresh interval in seconds (2–300)")
	namespace := flags.String("namespace", "", "initial namespace (empty for all)")
	sshUser := flags.String("ssh-user", "", "SSH login user (default: OpenSSH config)")
	collect := flags.Bool("collect", false, "record history and all pod logs without a terminal")
	history := flags.Bool("history", false, "export archived records as JSONL without connecting to the cluster")
	archiveDir := flags.String("archive-dir", "", "archive directory")
	archiveSearch := flags.String("archive-search", "", "full-text word/prefix search for --history")
	archiveKind := flags.String("archive-kind", "", "resource kind or logs for --history")
	cluster := flags.String("context", "", "Kubernetes context (defaults to current-context)")
	shell := flags.String("shell", "", "container shell executable")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	var c config
	var err error
	if selected != nil {
		c = selected.settings
		*cluster = selected.name
	} else {
		c, err = loadConfig(*path)
		if err != nil {
			return err
		}
		flags.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "theme":
				c.Theme = *theme
			case "refresh":
				c.RefreshSeconds = *interval
			case "namespace":
				c.Namespace = *namespace
			case "ssh-user":
				c.SSHUser = *sshUser
			case "shell":
				c.Shell = *shell
			case "archive-dir":
				c.ArchiveDir = *archiveDir
			}
		})
	}
	if err = c.validate(); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	if *cluster == "" {
		lookup, stop := context.WithTimeout(ctx, 2*time.Second)
		*cluster = currentContext(lookup)
		stop()
	}
	if *cluster == "" {
		return fmt.Errorf("cannot determine Kubernetes context; set --context or check kubectl config current-context")
	}
	dbPath := archivePath(c.ArchiveDir, *cluster)
	if *history {
		filter := archiveFilter{Namespace: c.Namespace, Kind: *archiveKind, Search: *archiveSearch}
		encoder := json.NewEncoder(os.Stdout)
		for {
			data, err := archiveRead(ctx, dbPath, archiveSQL(filter, true))
			if err != nil {
				return err
			}
			var entries []json.RawMessage
			if len(data) > 0 {
				if err = json.Unmarshal(data, &entries); err != nil {
					return err
				}
			}
			for _, entry := range entries {
				if err = encoder.Encode(entry); err != nil {
					return err
				}
				var row struct{ ID int64 }
				_ = json.Unmarshal(entry, &row)
				filter.Before = row.ID
			}
			if len(entries) < archivePageSize {
				return nil
			}
		}
	}
	var database *archive
	if c.ArchiveEnabled {
		database, err = openArchive(dbPath)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := database.close(); closeErr != nil {
				resultErr = errors.Join(resultErr, closeErr)
			}
		}()
	}
	api, err := startAPI(ctx, *cluster)
	if err != nil {
		return err
	}
	defer api.close()
	var recording *collector
	if database != nil {
		recording, err = startCollector(ctx, api, database, c.ArchiveStreams)
		if err != nil {
			return err
		}
	}
	defer func() { recording.close() }()
	if *collect {
		if database == nil {
			return fmt.Errorf("archive_enabled must be true for --collect")
		}
		if recording == nil {
			return fmt.Errorf("a collector is already recording this context")
		}
		interrupted := make(chan os.Signal, 1)
		signal.Notify(interrupted, syscall.SIGINT)
		defer signal.Stop(interrupted)
		fmt.Fprintln(os.Stderr, "Recording", *cluster, "to", dbPath)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastStatus := ""
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-interrupted:
				return nil
			case <-ticker.C:
				if err := database.failure(); err != nil {
					return err
				}
				if status := recording.status(); status != lastStatus {
					fmt.Fprintln(os.Stderr, status)
					lastStatus = status
				}
			}
		}
	}
	if c.ClusterSlots == [5]string{} {
		lookup, stop := context.WithTimeout(ctx, 2*time.Second)
		c.ClusterSlots = loadClusterSlots(lookup, *cluster)
		stop()
	}
	raw := func() error { _, err := stty("raw", "-echo", "min", "0", "time", "1"); return err }
	if term.state == "" {
		state, err := stty("-g")
		if err != nil {
			return fmt.Errorf("a terminal is required")
		}
		term.state = strings.TrimSpace(string(state))
		if err = raw(); err != nil {
			return err
		}
		enterTerminal()
	}
	a := newApp()
	a.config, a.configFile = c, *path
	a.context = *cluster
	a.archivePath = dbPath
	a.namespace = c.Namespace
	if c.HideSidebar {
		a.treeFocus = false
	}
	if c.Namespace != "" {
		a.expanded = map[string]bool{c.Namespace: true}
		a.snapshot.Namespaces = []string{c.Namespace}
		a.treeSelected = 3
	}
	a.rows, a.cols = terminalSize()
	term.stopSwitching()
	if selected != nil {
		a.switching = selected.name
	}
	a.draw()
	results := make(chan snapshot, 1)
	viewResults := make(chan viewResult, 128)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	refresh := func() {
		if a.loading {
			return
		}
		a.loading = true
		workers.Go(func() {
			loadCtx, stop := context.WithTimeout(ctx, 35*time.Second)
			defer stop()
			value := loadCluster(loadCtx, api)
			select {
			case results <- value:
			case <-ctx.Done():
			}
		})
	}
	podResults := make(chan podMetricResult, 1)
	var podCancel context.CancelFunc
	var podTarget string
	var podRequested time.Time
	podGeneration, podBusy := 0, false
	defer func() {
		if podCancel != nil {
			podCancel()
		}
	}()
	updatePodMetrics := func() {
		pod, ok := a.selected()
		target := ""
		if a.podPanelWidth() > 0 && ok {
			target = pod.id()
		}
		if target != podTarget {
			if podCancel != nil {
				podCancel()
			}
			podGeneration++
			podTarget, podBusy, podRequested = target, false, time.Time{}
			a.podMetrics, a.podMetricsStatus = nil, "Loading metrics…"
		}
		if target == "" || podBusy || time.Since(podRequested) < time.Duration(c.RefreshSeconds)*time.Second {
			return
		}
		podBusy, podRequested = true, time.Now()
		request, stop := context.WithTimeout(ctx, 10*time.Second)
		podCancel = stop
		var node record
		for _, candidate := range a.snapshot.Resources["nodes"] {
			if candidate.Name == pod.Node {
				node = candidate
				break
			}
		}
		gen := podGeneration
		workers.Go(func() {
			defer stop()
			metrics, err := loadPodMetrics(request, api, pod, node)
			select {
			case podResults <- podMetricResult{generation: gen, metrics: metrics, err: err}:
			case <-ctx.Done():
			}
		})
	}
	var shellEnded func(error)
	closeShell := func(err error) {
		if a.shell == nil {
			return
		}
		a.shell.close()
		a.shell = nil
		a.previousFrame = nil
		if shellEnded != nil {
			shellEnded(err)
			shellEnded = nil
		}
		a.status = "Session ended"
		if err != nil {
			a.status = "Shell failed: " + safeText(err.Error())
		}
	}
	defer func() { closeShell(nil) }()
	defer func() {
		for _, v := range a.outputs {
			v.stop()
		}
		for v := a.view; v != nil; v = v.parent {
			v.stop()
		}
	}()
	generation := 0
	startViewer := func(v *viewer) {
		if v.kind == "values" && v.background == nil {
			v.background, v.backgroundCols = a.frame(), a.cols
		}
		if v.isWorkspaceView() {
			for _, open := range a.outputs {
				if open != v && open.kind == v.kind && open.resource == v.resource && open.target.id() == v.target.id() {
					a.view, a.treeFocus = open, false
					return
				}
			}
		} else if a.view != nil && !a.view.isWorkspaceView() && a.view != v.parent {
			a.view.stop()
		}
		v.stop()
		generation++
		v.generation = generation
		v.lines = nil
		v.stats = nil
		v.details, v.detailStatus = nil, "Loading description…"
		v.entries = nil
		v.values = nil
		v.selected = 0
		v.top = 0
		v.done = false
		v.status = "Connecting…"
		a.view = v
		if v.isWorkspaceView() {
			if !slices.Contains(a.outputs, v) {
				a.outputs = append(a.outputs, v)
			}
			a.lastOutput, a.treeFocus = v, false
		}
		viewCtx, stop := context.WithCancel(ctx)
		if v.kind != "stats" && (v.kind != "logs" || v.previous) {
			stop()
			viewCtx, stop = context.WithTimeout(ctx, 20*time.Second)
		}
		viewDone := make(chan struct{})
		v.cancel, v.workerDone = stop, viewDone
		if v.kind == "stats" {
			detailCtx, stopDetails := context.WithTimeout(viewCtx, 20*time.Second)
			cmd, commandErr := a.command(detailCtx, "describe", "nodes", v.target, 0, false)
			gen := v.generation
			go func() {
				defer close(viewDone)
				var tasks sync.WaitGroup
				tasks.Go(func() {
					pollNodeStats(viewCtx, api, v.target, time.Duration(c.RefreshSeconds)*time.Second, gen, viewResults)
				})
				tasks.Go(func() {
					defer stopDetails()
					result := viewResult{generation: gen, description: true, err: commandErr}
					if result.err == nil {
						output, err := cmd.CombinedOutput()
						result.lines = strings.Split(strings.TrimRight(string(output), "\n"), "\n")
						result.err = err
					}
					select {
					case viewResults <- result:
					case <-viewCtx.Done():
					}
				})
				tasks.Wait()
			}()
			return
		}
		if v.kind == "archive" || v.kind == "archive-detail" {
			filter, id, kind, gen := v.archiveFilter, v.entryID, v.kind, v.generation
			go func() {
				defer close(viewDone)
				result := viewResult{generation: gen, done: true}
				if kind == "archive" {
					result.entries, result.err = readArchive(viewCtx, dbPath, filter)
				} else {
					result.lines, result.err = archiveDetail(viewCtx, dbPath, id)
				}
				select {
				case viewResults <- result:
				case <-viewCtx.Done():
				}
			}()
			return
		}
		cmd, err := a.command(viewCtx, v.kind, v.resource, v.target, v.container, v.previous)
		if err != nil {
			v.lines = []string{err.Error()}
			v.status = "Unavailable"
			v.done = true
			close(viewDone)
			return
		}
		if v.kind == "values" {
			go func() {
				defer close(viewDone)
				result := viewResult{generation: v.generation, done: true}
				data, err := cmd.Output()
				if err != nil {
					result.err = err
					if exitErr, ok := err.(*exec.ExitError); ok {
						result.err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
					}
				} else {
					result.lines, result.values, result.err = valueLines(data, v.resource)
				}
				select {
				case viewResults <- result:
				case <-viewCtx.Done():
				}
			}()
			return
		}
		go func() { defer close(viewDone); streamCommand(viewCtx, cmd, v.generation, viewResults) }()
	}
	inputCtx, stopInput := context.WithCancel(ctx)
	keys := readInput(inputCtx)
	defer func() {
		stopInput()
		for range keys {
		}
	}()
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, syscall.SIGINT)
	defer signal.Stop(interrupted)
	ticker := time.NewTicker(time.Duration(c.RefreshSeconds) * time.Second)
	defer ticker.Stop()
	paint := time.NewTicker(100 * time.Millisecond)
	defer paint.Stop()
	dirty := true
	refresh()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-interrupted:
			return nil
		case value := <-results:
			a.applySnapshot(value)
			a.switching = ""
			dirty = true
		case result := <-podResults:
			if result.generation == podGeneration {
				podBusy = false
				a.podMetrics = result.metrics
				a.podMetricsStatus = ""
				if result.err != nil {
					a.podMetricsStatus = "Metrics unavailable: " + safeText(result.err.Error())
				}
				dirty = true
			}
		case event := <-a.shellEvents():
			if len(event.data) == 0 {
				closeShell(event.err)
			} else if err := a.shell.receive(event); err != nil {
				closeShell(err)
			}
			dirty = true
		case result := <-viewResults:
			v := a.view
			for _, open := range a.outputs {
				if open.generation == result.generation {
					v = open
					break
				}
			}
			if v != nil && v.generation == result.generation && result.description {
				v.details, v.detailStatus = result.lines, "Description"
				if result.err != nil {
					v.detailStatus = "Description unavailable: " + safeText(result.err.Error())
				}
				dirty = true
				continue
			}
			if v != nil && v.generation == result.generation && v.kind == "stats" {
				v.stats = result.stats
				v.status = "Live node stats"
				if result.err != nil {
					v.status = "Stats unavailable: " + safeText(result.err.Error())
				}
				if v.stats != nil && !v.stats.at.IsZero() {
					v.status += " · sample " + v.stats.at.Local().Format("15:04:05")
				}
				dirty = true
				continue
			}
			if v != nil && v.generation == result.generation {
				if result.done {
					if v.kind == "archive" {
						v.entries = result.entries
						v.lines = nil
						v.selected = 0
						for _, e := range result.entries {
							v.lines = append(v.lines, fmt.Sprintf("%s  %-12s %-10s %s/%s %s %s", e.At, e.Kind, e.Operation, e.Namespace, e.Subject, e.Container, e.Message))
						}
					} else if v.kind == "archive-detail" || v.kind == "values" {
						v.lines = result.lines
						v.values = result.values
					}
					v.done = true
					v.status = "Complete"
					if result.err != nil {
						v.status = "Command failed"
						v.appendLine(result.err.Error(), a.config.LogLines)
					}
				} else {
					v.status = "Streaming"
					v.appendLine(result.line, a.config.LogLines)
				}
				dirty = true
			}
		case <-ticker.C:
			if database != nil && recording == nil {
				recording, err = startCollector(ctx, api, database, c.ArchiveStreams)
				if err != nil {
					a.status = err.Error()
				}
			}
			if !a.paused {
				refresh()
				dirty = true
			}
		case <-paint.C:
			updatePodMetrics()
			if a.switching != "" {
				a.spinner++
				dirty = true
			}
			archiveStatus := "Archive disabled"
			if database != nil {
				archiveStatus = "Archive · external collector"
				if recording != nil {
					archiveStatus = recording.status()
				}
				if err := database.failure(); err != nil {
					archiveStatus = "ARCHIVE FAILED · " + err.Error()
				}
			}
			if archiveStatus != a.archiveStatus {
				a.archiveStatus = archiveStatus
				dirty = true
			}
			if dirty {
				a.draw()
				dirty = false
			}
		case <-resized:
			a.rows, a.cols = terminalSize()
			a.previousFrame = nil
			dirty = true
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			before := a.config.RefreshSeconds
			quit, manual := a.handle(k)
			if quit {
				return nil
			}
			if before != a.config.RefreshSeconds {
				ticker.Reset(time.Duration(a.config.RefreshSeconds) * time.Second)
				a.status = fmt.Sprintf("Refresh every %ds · w to save", a.config.RefreshSeconds)
			}
			if manual {
				refresh()
			}
			action := a.action
			a.action = ""
			if next, ok := strings.CutPrefix(action, "cluster:"); ok {
				term.startSwitching(a, next)
				return &clusterSwitch{name: next, settings: a.config}
			}
			switch action {
			case "":
			case "close-shell":
				closeShell(nil)
			case "close":
				a.closeViewer(a.view)
			case "close-output":
				a.closeViewer(a.closeTab)
				a.closeTab = nil
			case "copy-value":
				if v := a.view; v != nil && v.kind == "values" && v.selected >= 0 && v.selected < len(v.values) {
					value := v.values[v.selected]
					copyCtx, stop := context.WithTimeout(ctx, 3*time.Second)
					err := copyValue(copyCtx, value.raw, v.resource == "secrets")
					stop()
					v.status = "Copied " + safeText(value.name)
					if err != nil {
						v.status = "Copy failed: " + safeText(err.Error())
					}
				}
			case "reload":
				if a.view != nil {
					startViewer(a.view)
				}
			case "save":
				a.config.Namespace = a.namespace
				if err := a.config.save(a.configFile); err != nil {
					a.status = "Save failed: " + err.Error()
				} else {
					a.status = "Saved " + a.configFile
				}
			case "archive", "resource-archive":
				if database == nil {
					a.status = "Archive is disabled in settings"
					break
				}
				filter := archiveFilter{Namespace: a.namespace}
				if action == "resource-archive" {
					r, ok := a.selected()
					if !ok || a.treeFocus {
						a.status = "Select a resource row first"
						break
					}
					filter.Namespace = r.Namespace
					for _, kind := range resourceTypes {
						if kind.key == a.resource {
							filter.Subject = kind.kind + "/" + r.Name
						}
					}
					if a.resource == "events" {
						filter.Subject = r.Cells[0]
					}
				}
				startViewer(&viewer{kind: "archive", title: "Permanent archive · " + first(filter.Subject, first(filter.Namespace, "all namespaces")), archiveFilter: filter})
			case "archive-detail":
				if v := a.view; v != nil && v.selected < len(v.entries) {
					startViewer(&viewer{kind: "archive-detail", title: "Archived record", entryID: v.entries[v.selected].ID, parent: v})
				}
			case "help":
				a.view = &viewer{title: "Keyboard & settings", kind: "help", status: a.configFile, lines: strings.Split(helpText+clusterHelp(a.config.ClusterSlots), "\n"), done: true}
			default:
				r, ok := a.selected()
				resource := a.resource
				if action == "edit" && a.view != nil {
					r, resource, ok = a.view.target, a.view.resource, true
				}
				if !ok || a.treeFocus && a.view == nil {
					a.status = "Select a resource row first"
					break
				}
				if action == "shell" && a.resource == "nodes" {
					action = "ssh"
				}
				if action == "shell" || action == "ssh" || action == "edit" {
					cmd, err := a.command(ctx, action, resource, r, a.container, false)
					if err != nil {
						a.status = err.Error()
						break
					}
					session := archiveKey(time.Now().String() + r.id())
					recordSession := func(operation string, sessionErr error) {
						if database == nil {
							return
						}
						message := action + " " + r.Namespace + "/" + r.Name
						if sessionErr != nil {
							message += " · " + sessionErr.Error()
						}
						data, _ := json.Marshal(map[string]any{"action": action, "context": a.context, "target": r.Name, "namespace": r.Namespace, "container": containerName(r, a.container), "arguments": cmd.Args, "outcome": fmt.Sprint(sessionErr)})
						subject := a.resource + "/" + r.Name
						for _, kind := range resourceTypes {
							if kind.key == a.resource {
								subject = kind.kind + "/" + r.Name
								break
							}
						}
						if writeErr := database.add(ctx, archiveEntry{Key: session + "/" + operation, Kind: "sessions", Namespace: r.Namespace, Subject: subject, Operation: operation, Message: message, Data: string(data)}); writeErr != nil {
							a.status = writeErr.Error()
						}
					}
					if action == "shell" {
						_, _, w, h := a.shellBounds()
						recordSession("started", nil)
						a.shell, err = newInteractiveTerminal(cmd, h-3, w-2)
						if err != nil {
							recordSession("ended", err)
							a.status = "Shell failed: " + safeText(err.Error())
							break
						}
						a.shellTitle = "Shell · " + r.Namespace + " / " + r.Name + " · " + first(containerName(r, a.container), "default")
						shellEnded = func(err error) { recordSession("ended", err) }
						a.menu, a.searching = nil, false
						break
					}
					stopInput()
					for range keys {
					} // Join the reader before giving the child exclusive terminal input.
					leaveTerminal()
					if _, err = stty(term.state); err != nil {
						return err
					}
					fmt.Printf("KiwiKube · %s · %s/%s\nExit the session to return.\n", action, safeText(r.Namespace), safeText(r.Name))
					cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
					recordSession("started", nil)
					err = cmd.Run()
					recordSession("ended", err)
					select {
					case <-interrupted:
					default:
					}
					if rawErr := raw(); rawErr != nil {
						return rawErr
					}
					enterTerminal()
					a.previousFrame = nil
					a.rows, a.cols = terminalSize()
					inputCtx, stopInput = context.WithCancel(ctx)
					defer stopInput()
					keys = readInput(inputCtx)
					a.status = "Session ended"
					if err != nil {
						a.status = "Session failed: " + err.Error() + " (see terminal scrollback)"
					}
					if action == "edit" {
						if err == nil {
							a.status = "Edit complete"
							if a.view != nil {
								startViewer(a.view)
							}
							refresh()
						} else if a.view != nil {
							a.view.status = "Edit failed: " + err.Error() + " (see terminal scrollback)"
						}
					}
				} else {
					startViewer(&viewer{title: action + " · " + first(r.Namespace, "cluster") + " / " + r.Name, kind: action, resource: a.resource, target: r, container: a.container, follow: action == "logs"})
				}
			}
			updatePodMetrics()
			a.draw()
			dirty = false
		}
	}
}
func stty(args ...string) ([]byte, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	return cmd.Output()
}
