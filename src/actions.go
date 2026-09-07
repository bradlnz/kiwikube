package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type viewResult struct {
	description bool
	stats       *nodeStats
	generation  int
	entries     []archiveEntry
	lines       []string
	values      []displayValue
	line        string
	done        bool
	err         error
}

func containerName(r record, index int) string {
	if len(r.Containers) == 0 {
		return ""
	}
	return r.Containers[index%len(r.Containers)]
}
func (a *app) command(ctx context.Context, kind, resource string, r record, container int, previous bool) (*exec.Cmd, error) {
	target := resource + "/" + r.Name
	args := []string{"-n", r.Namespace}
	if r.Namespace == "" {
		args = nil
	}
	switch kind {
	case "describe":
		args = append(args, "describe", target)
	case "yaml":
		args = append(args, "get", target, "-o", "yaml")
	case "values":
		if resource != "secrets" && resource != "configmaps" {
			return nil, fmt.Errorf("Select a Secret or ConfigMap to view values")
		}
		args = append(args, "get", target, "-o", "json")
	case "edit":
		if resource != "secrets" && resource != "configmaps" {
			return nil, fmt.Errorf("Select a Secret or ConfigMap to edit values")
		}
		args = append(args, "edit", target)
	case "history":
		if resource != "deployments" && resource != "statefulsets" && resource != "daemonsets" {
			return nil, fmt.Errorf("Select a deployment, StatefulSet, or DaemonSet for rollout history")
		}
		args = append(args, "rollout", "history", target)
	case "logs", "shell":
		if resource != "pods" && (kind != "logs" || resource != "jobs") {
			return nil, fmt.Errorf("Select a pod or Job for logs; container shells require a pod")
		}
		if kind == "shell" {
			args = append(args, "exec", "-it", r.Name)
		} else {
			logTarget := r.Name
			if resource == "jobs" {
				logTarget = "job/" + r.Name
			}
			args = append(args, "logs", logTarget, "--timestamps", "--tail="+strconv.Itoa(a.config.LogLines))
			if resource == "jobs" {
				args = append(args, "--all-pods=true", "--all-containers=true")
			}
			if previous {
				args = append(args, "--previous")
			} else {
				args = append(args, "--follow")
			}
		}
		if name := containerName(r, container); name != "" && resource == "pods" {
			args = append(args, "-c", name)
		}
		if kind == "shell" {
			args = append(args, "--", a.config.Shell)
		}
	case "ssh":
		node := r
		if resource != "nodes" {
			if r.Node == "" {
				return nil, fmt.Errorf("Select a node or a scheduled pod for SSH")
			}
			node = record{}
			for _, candidate := range a.snapshot.Resources["nodes"] {
				if candidate.Name == r.Node {
					node = candidate
					break
				}
			}
		}
		host := first(node.Host, node.Name)
		if !validSSHHost(host) {
			return nil, fmt.Errorf("No valid SSH address available for this node")
		}
		args = []string{"-t"}
		if a.config.SSHUser != "" {
			args = append(args, "-l", a.config.SSHUser)
		}
		args = append(args, "--", host)
		return exec.CommandContext(ctx, "ssh", args...), nil
	default:
		return nil, fmt.Errorf("Unknown action: %s", kind)
	}
	if kind != "shell" && kind != "logs" {
		args = append(args, "--request-timeout=10s")
	}
	return clusterCommand(ctx, a.context, args...), nil
}
func validSSHHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "-") {
		return false
	}
	for _, c := range host {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(".-:", c)) {
			return false
		}
	}
	return true
}

type displayValue struct {
	name, raw  string
	start, end int
}

func valueLines(data []byte, resource string) ([]string, []displayValue, error) {
	var value struct{ Data, BinaryData map[string]string }
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, nil, fmt.Errorf("decode values: %w", err)
	}
	values := map[string]string{}
	for name, text := range value.Data {
		values[name] = text
	}
	encoded := value.BinaryData
	if resource == "secrets" {
		encoded = value.Data
	}
	for name, text := range encoded {
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid base64 in value %q", name)
		}
		values[name] = string(decoded)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	var entries []displayValue
	for _, name := range names {
		text := values[name]
		entry := displayValue{name: name, raw: text, start: len(lines)}
		if !utf8.ValidString(text) || strings.ContainsFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
			text = "base64: " + base64.StdEncoding.EncodeToString([]byte(text))
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(text), "", "  ") == nil {
			text = pretty.String()
		}
		lines = append(lines, safeText(name)+": |")
		if text == "" {
			text = `""`
		}
		for _, line := range strings.Split(text, "\n") {
			lines = append(lines, "  "+safeText(strings.ReplaceAll(line, "\t", "  ")))
		}
		lines = append(lines, "")
		entry.end = len(lines)
		entries = append(entries, entry)
	}
	return lines, entries, nil
}

func copyValue(ctx context.Context, value string, sensitive bool) error {
	args := []string{"--type", "text/plain"}
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		args[1] = "application/octet-stream"
	}
	if sensitive {
		args = append(args, "--sensitive")
	}
	cmd := exec.CommandContext(ctx, "wl-copy", args...)
	cmd.Stdin = strings.NewReader(value)
	return cmd.Run()
}
func streamCommand(ctx context.Context, cmd *exec.Cmd, generation int, results chan<- viewResult) {
	send := func(r viewResult) bool {
		select {
		case results <- r:
			return true
		case <-ctx.Done():
			return false
		}
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		send(viewResult{generation: generation, done: true, err: err})
		return
	}
	stopRead := context.AfterFunc(ctx, func() { _ = pipe.Close() })
	defer stopRead()
	cmd.Stderr = cmd.Stdout
	if err = cmd.Start(); err != nil {
		pipe.Close()
		send(viewResult{generation: generation, done: true, err: err})
		return
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Bound retained bytes as well as line count, including pathological application logs.
		if len(line) > 4096 {
			line = line[:4096] + " … [line truncated]"
		}
		if !send(viewResult{generation: generation, line: safeText(line)}) {
			break
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = cmd.Process.Kill()
	}
	err = cmd.Wait()
	if scanErr != nil {
		err = fmt.Errorf("reading output (maximum line 1 MiB): %w", scanErr)
	}
	send(viewResult{generation: generation, done: true, err: err})
}
func (v *viewer) appendLine(line string, limit int) {
	if len(v.lines) >= limit {
		v.lines[0] = ""
		v.lines = v.lines[1:]
		if !v.follow {
			v.top = max(0, v.top-1)
		}
	}
	v.lines = append(v.lines, line)
}

const helpText = `NAVIGATE
  ↑ ↓ / j k       Move selection          Enter       Pod/Job logs, node stats, values
  Tab             Resources / Logs tabs (or tree focus without logs)
  y               View YAML
  Ctrl+F / /      Search modal            d           Describe resource
  Alt+1–5         Switch cluster          H           Rollout history
  Click           Open tree / select row; click selected row to open
  Top menus       File / Edit / View / Help; live status and counts below
  Bottom labels   Click to run the displayed action, including shell
  F / Flow tab    Live Ingress → Service → Pod routing map
  m (Pods)        Toggle selected pod CPU/memory side panel
  Enter (Nodes)   CPU/memory/disk/traffic gauges; traffic uses observed peak
  Mouse wheel     Scroll hovered pane; click Esc back to close a viewer
  Esc             Clear search / back     a           All namespaces
  PgUp / PgDn     Move one page           q           Quit / close viewer

LOGS & ACCESS
  Logs stay beside the sidebar; Resources / Logs tabs keep the stream open.
  × / Esc         Close logs; opening another pod or Job replaces the logs tab
  Colors          Error/fatal red, warning amber, info accent, debug muted
  L               Follow pod/Job logs     c           Cycle pod container
  s               Pod shell / node SSH    S           SSH to pod's node
  In logs: p      Previous container logs f / G       Follow latest output
  Ctrl+F / /      Filter viewer lines     r           Reload / reconnect
  In viewer: ← →  Scroll horizontally     g           Jump to start
  Pod shells open in a large modal; Ctrl+C interrupts, Ctrl+] closes it.
  Exit the remote shell to return to KiwiKube.

EVENTS & CONFIGURATION
  T / K           Time / type dropdowns; click or use arrows and Enter
  X               Reset event filters
  Ctrl+F          Search event messages, reasons, and object names
  Enter           Secret / ConfigMap values popup; Esc closes it
  Secret text values are decoded; binary values stay base64. No Secret archive.
  Click / Tab     Select key              c           Copy decoded value
  e               Edit native YAML using current Kubernetes permissions
  Secret YAML data remains base64. Save and exit the editor to reload.
  Configuration, YAML, and Describe use syntax colors; arrows scroll and pan.

CUSTOMIZE
  t               Cycle kiwi/ocean/amber  b           Toggle sidebar
  [ / ]           Resize sidebar          - / +       Refresh faster / slower
  Space           Pause cluster refresh   r           Refresh now
  w               Save settings and namespace to config.json

ARCHIVE
  A               Search all archived history in this namespace
  v               History and logs for the selected resource
  In archive: n/N Older / newer page; Enter opens the full stored record
  Ctrl+F, Enter   Search the entire archive by words / prefixes
  Run --collect for recording without a terminal; --history exports JSONL.
Denied resources show their error and retain the last successful snapshot.
SSH uses your normal OpenSSH config, keys, and host verification.
`
