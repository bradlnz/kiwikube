# KiwiKube guide

[← README](../README.md) · [Controls](#navigation-and-controls) · [Customization](#customization) · [History](#permanent-history) · [Contributing](#contributing)

Run commands from the repository root.

## Requirements

KiwiKube uses Go, native `kubectl` / `sqlite3` tools, and libvterm for embedded pod shells.
Building requires a C compiler, pkg-config, and libvterm 0.3+ development headers
(Arch: `libvterm pkgconf`; Debian/Ubuntu: `libvterm-dev pkg-config`).

Requires Go 1.27 to build; `kubectl`, `sqlite3` (3.38+ with FTS5), `stty`, and a
Linux terminal to run. Node sessions also require `ssh`. The headless collector needs
only `kubectl` and `sqlite3`. Set `archive_enabled` to false to run without SQLite.

## Navigation and controls

KiwiKube uses `KUBECONFIG` when set, otherwise detects `~/.kube/config`
(including when kubectl is provided by K3s). It pins the current context for
all requests and actions to the selected context. Click a numbered slot at the
top right or press **Alt+1–5** to switch. The active slot is highlighted; unused
slots are disabled. Switching closes the current viewer, flushes the
old archive, and reconnects to the new context without changing kubeconfig's
global current-context. A **Switching cluster** modal animates until the new resource
list loads. Shortcuts work inside search and viewers. Inside a remote shell, keys belong
to that shell until you exit back to the dashboard.

Browse all namespaces or expand a specific namespace. Views cover pods,
deployments, StatefulSets, DaemonSets, Jobs, CronJobs, services, ingresses,
persistent volume claims, events, Secrets, ConfigMaps, and nodes. Node CPU and memory columns
show capacity. Describe and YAML views expose the full resource details;
deployments and related controllers also have a rollout history view.

| Key | Action |
| --- | --- |
| Alt+1–5 / click top-right slot | Switch Kubernetes context |
| Arrows / `j` `k` | Navigate |
| Enter | Open tree entry, pod/Job logs, node stats, Secret/ConfigMap values, or other resource details |
| Click | Open tree entry / select resource; click selected row again to open |
| Mouse wheel | Scroll the pane under the pointer or the open viewer |
| Tab | Switch Resources / Logs tabs when logs are open; otherwise tree / resources |
| Ctrl+F / `/` | Open search modal across fields; filters output inside viewers |
| Esc | Clear search / close viewer |
| `d` / `y` | Describe / YAML |
| `H` | Deployment, StatefulSet, or DaemonSet rollout history |
| `A` / `v` | Permanent namespace archive / selected resource's archive |
| `L` | Follow selected pod or Job's output logs |
| `c` | Cycle pod containers, including init and ephemeral containers |
| `s` | Interactive pod shell; SSH when a node is selected |
| `m` | Toggle selected pod CPU/memory side panel on the Pods screen |
| `S` | SSH into selected pod's node or selected node |
| `a` | Show all namespaces |
| Space / `r` | Pause automatic refresh / refresh now |
| `?` / `q` | Help / quit (closes an open viewer first) |

### Tabs, logs, and shells

The top **File / Edit / View / Help** menus and bold bottom action labels are clickable,
including **s shell**. Live status and cluster counts sit at the bottom. Namespace headings and resource
identities have distinct colors, retained through refreshes and namespace additions. Logs open beside the
sidebar in a reusable **Logs** tab. Click **Resources** or press Tab to browse while
the log stream continues; click its named **Logs: resource** tab to return, or **×** / Esc to close it.
Tabs align above the content pane with Kiwicode-style padding, muted inactive
labels, and a bold accent on the active tab. Logs and Flow appear when opened;
the active tab stays visible in narrow windows. Each **×** closes only its tab.
Opening another pod or Job replaces the logs tab. Job logs include all current
pods and containers, with source prefixes. Errors/fatal messages appear red,
warnings amber, info in the theme accent, and debug/trace in muted text.
Click the search row (the log status row when viewing logs) to edit the filter,
or use Ctrl+F to open the search modal. Enter applies; Esc clears. Other viewers have a clickable **Esc back** label.
In viewers, use PgUp/PgDn to scroll, left/right to pan long lines, `g` to jump
to the beginning, and `f`/`G` to follow the latest output. Scrolling up stops
autoscroll while logs continue arriving. In logs, `p` switches to the previous
container instance, `c` switches containers, and `r` reconnects. Exit an
interactive shell normally to return to the dashboard. Pod shells run inside a
large modal with terminal colors, cursor movement, resize support, and wheel
scrollback. Ctrl+C interrupts the foreground command; **Ctrl+]** or the modal
close button ends the shell. SSH and the native configuration editor retain
their full-terminal sessions.

### Node and pod metrics

Enter on a node opens a live **Node: name** tab with green-to-red CPU, memory,
disk, receive, and transmit gauges alongside a formatted node description.
On narrow screens the description follows the gauges; scroll to read it.
Descriptions load independently, so denied metrics access still leaves the node
details available. **r** reloads both. CPU/memory/disk use reported capacity.
Traffic is a counter delta per second, scaled to the observed peak since opening
(the label states this scale; it does not imply link saturation). A fresh second
sample is needed for traffic, and missing metrics or denied access are explicit.
The view uses the [Kubelet summary API](https://kubernetes.io/docs/reference/kubelet-api/stats.v1alpha1/)
through your existing Kubernetes context and permissions.

On **Pods**, press **m** or click **m metrics** to show the selected pod's CPU and
memory at the right. Select another pod to update the panel; **×** or **m** hides
it. The panel requires room for both the table and metrics (hide the sidebar or
widen a compact terminal). Usage comes from `metrics.k8s.io`; gauges compare with
pod limits, or node capacity when no complete limit is set. Permission failures
and missing metrics-server data are shown without substituting zero usage.

### Events and configuration

Events have dropdown filters: **All / 5m / 15m / 1h / 6h / 24h**, event type,
search, and Reset. **T** opens time windows, **K** opens All/Warning/Normal;
click an option or use arrows and Enter,
and **X** clears the filters. Windows use each retained event's latest occurrence;
type and text filters combine with the namespace selected in the sidebar.

Enter on a Secret or ConfigMap opens a scrollable values popup. Secret text values
are decoded, binary values remain base64, and JSON configuration is indented.
Describe opens as a named workspace tab beside the namespace sidebar, keeping
the top menu and bottom actions visible. Resources / Describe tabs preserve
your search and scroll position; **×** closes the description.
Describe views use bordered sections, aligned fields, highlighted values, and
wrapped text for annotations and tables. Ctrl+F searches the formatted details.
Configuration values and YAML color keys, quoted strings, numbers, booleans,
and comments. Use Ctrl+F to search, arrows to scroll/pan, and Esc or
the popup's close label to return. Click a value or press Tab to select its key,
then **c Copy** to copy the exact decoded bytes through `wl-copy` (Secret copies
are marked sensitive). **e Edit** opens the native `kubectl edit` YAML editor,
using your current context and normal RBAC/admission policies. Secret YAML data
remains base64; save and exit to reload the popup. Secret contents are never
written to the archive.

### Flow graph

The **Flow** tab (**F**, or View → Flow graph) shows live Ingress → Service → Pod
routing relationships for the selected namespace, using Ingress backends and
Service label selectors. It shows missing backends and unmatched pods, and
refreshes with cluster data. This is a routing map, not measured traffic.

## Customization

Settings load from `$XDG_CONFIG_HOME/kiwikube/config.json`, normally
`~/.config/kiwikube/config.json`. Defaults:

```json
{
  "theme": "kiwi",
  "refresh_seconds": 5,
  "hide_sidebar": false,
  "sidebar_width": 26,
  "log_lines": 2000,
  "namespace": "",
  "shell": "/bin/sh",
  "ssh_user": "",
  "cluster_slots": ["", "", "", "", ""],
  "archive_enabled": true,
  "archive_dir": "/home/you/.local/state/kiwikube",
  "archive_streams": 32
}
```

Use `t` to cycle kiwi/ocean/amber themes, `b` to toggle the sidebar, `[`/`]`
to resize it, and `-`/`+` to decrease/increase the refresh interval. Press `w`
to save your settings and current namespace. On narrow terminals, Tab switches
between full-width tree and resource views.

With empty `cluster_slots`, KiwiKube fills the five slots from kubeconfig,
including the active context. Pin their order with, for example,
`"cluster_slots": ["dev", "staging", "production", "", ""]`. Press `w` to save
the detected slots and other settings. `?` shows full context names.

The actual archive directory defaults to `$XDG_STATE_HOME/kiwikube`, or
`~/.local/state/kiwikube`. Each context gets its own SQLite database.

CLI overrides: `--context NAME`, `--archive-dir PATH`, `--config PATH`, `--theme ocean`, `--refresh 10`,
`--namespace production`, `--shell /bin/bash`, and `--ssh-user admin`.
An empty namespace means all namespaces. Refresh intervals are limited to
2–300 seconds, sidebar widths to 18–50 columns, and retained output to
100–10,000 lines. The shell setting names one executable, not a command string.

SSH chooses the node's external IP, then internal IP, then hostname, and uses
your normal OpenSSH configuration, keys, and host verification. An empty
`ssh_user` leaves user selection to OpenSSH. Nodes need reachable SSH access;
pod shells need Kubernetes exec permission and a shell in the selected container.

## Permanent history

Recording is on by default and independent of which resource or logs you open.
It stores all supported resource changes, deletion records, complete resource
JSON (including deployment specifications), retained events, current and previous
container logs, and KiwiKube shell/SSH session starts and outcomes. Regular, init,
and ephemeral containers are discovered automatically. Session records contain
connection metadata, not terminal transcripts or commands typed inside shells.
This is resource history, not the Kubernetes API server's separate audit log.

Records have no automatic expiry. Files are private (0600), and a full disk or
write failure is surfaced instead of silently dropping or deleting history.
Back up the local database with SQLite's `.backup` command; it includes committed
WAL contents while recording continues. Use a local filesystem for the WAL archive.

Press `A` for the namespace archive, or `v` for a selected resource's history,
related events, and logs. Use `/` then Enter to search indexed message/status words
and prefixes across the full archive. `K` cycles all/logs/events/deployments/
sessions/collector issues; `n`/`N` page older/newer records; Enter opens the full
original record. Results are ordered by capture ID, newest first. Source log
timestamps are retained. Search does not scan entire manifest payloads; those
remain available in the record detail and export.

```sh
# Record without a terminal. Stop with Ctrl+C; pending batches are flushed.
./kiwikube --collect --context my-cluster

# Read/export persisted history even when the cluster is offline.
./kiwikube --history --context my-cluster > history.jsonl
./kiwikube --history --context my-cluster --namespace production \
  --archive-kind logs --archive-search 'request failed' > failures.jsonl
```

The dashboard records its selected cluster while it runs. Switching clusters or
quitting stops that in-process collector. Run `--collect` separately to continue
recording a cluster while the UI is closed or viewing another cluster. A per-context
lock prevents duplicate collectors; dashboards can share the archive with a
headless collector and take over if it exits.

A systemd user service template is included for background collection:

```sh
install -Dm755 ./kiwikube ~/.local/bin/kiwikube
install -Dm644 systemd/kiwikube-archive@.service \
  ~/.config/systemd/user/kiwikube-archive@.service
systemctl --user daemon-reload
systemctl --user enable --now "$(systemd-escape --template=kiwikube-archive@.service my-cluster)"
```

It uses the same settings and kubeconfig detection as the dashboard. If you use
an explicit `KUBECONFIG`, set it in the unit's environment. User services run with
the user service manager; continuing after logout requires user lingering.

Resource watches reconnect from durable resource versions. If Kubernetes has
expired a watch version, collection relists and records the gap. Log reconnects
resume with a timestamp overlap; deduplication preserves repeated identical
lines at the same consecutive timestamp. Logs already rotated away upstream,
resources deleted before initial collection, and data lost during an upstream
history gap cannot be reconstructed. Keep the collector running for continuous
coverage. [Kubernetes watch semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/).

## Performance

One private Unix-socket Kubernetes proxy shares authentication and HTTP
connections across requests. Resource archives use long-lived watches; dashboard
refreshes make at most four HTTP list requests concurrently, with paginated
responses. No kubectl subprocess is spawned per resource refresh or archived
log line. Filtering is local, table formatting only touches visible rows, and
unchanged terminal lines are not repainted. Stream rendering is capped at 10 fps.

A persistent SQLite writer commits batches every 250 ms, 256 records, or about
1 MiB, whichever comes first. WAL plus `synchronous=FULL` makes committed batches
durable; resource versions and log cursors commit in the same transaction as
their records. Abrupt termination can lose the uncommitted batch, which is
replayed on reconnect where the upstream still retains it. Writes apply
backpressure through a bounded queue. Indexed keyset paging and FTS5 search
avoid full-history scans, offsets, and sorting all matches.
[SQLite WAL](https://www.sqlite.org/wal.html), [FTS5](https://www.sqlite.org/fts5.html).

`archive_streams` limits concurrent background log requests (1–512, default 32).
The status line shows active and queued streams. One-minute request rotation
shares slots with queued containers. Increase the limit for clusters with more
active containers when continuous coverage needs more connections; API/log
retention still limits catch-up. A log line over 1 MiB or archived record over
2 MiB produces a visible capture issue. Original accepted records are stored
intact; only live viewer output and archive summaries are shortened for display.
Control characters are neutralized when rendered.

Measured on the development machine (Core i5-8350U; synthetic data, one run):

| Check | Result |
| --- | --- |
| Durable 256-record batches | ~17,600 records/second |
| Recent/resource page, 100,000 archived records | ~4.6–5.0 ms |
| Common-word search page, 100,000 records | ~21 ms |
| Frame with 10,000 resource rows | ~0.61 ms |

These include the native SQLite reader process for queries. Actual rates depend
on log sizes, storage, and cluster load; the commands below reproduce the checks.

## Contributing

Bug reports, documentation improvements, and pull requests are welcome.
[Open an issue](https://github.com/bradlnz/kiwikube/issues) with steps to reproduce,
your KiwiKube revision, and relevant terminal and Kubernetes versions. Remove
credentials and private cluster data from any logs or screenshots you share.

For code changes, fork the repository, create a branch, keep the change focused,
and run the checks below before opening a pull request. Include a regression check
for bug fixes and update the relevant documentation when behavior changes.

### Checks

```sh
./build.sh
go vet ./...
go test -race ./...
python3 tests/terminal_smoke.py
go test ./src -run '^$' -bench 'BenchmarkArchive|BenchmarkVisibleFrame' -benchmem
```

The terminal smoke test uses Python's standard library and a real PTY with
isolated fake kubectl/SSH/clipboard commands. It checks menus, search modal, flow
routes, event dropdowns, exact Secret copying, successful and denied edits,
log following, container selection,
previous logs, shell input handoff, SSH, settings, resizing, partial permissions,
terminal restoration, background archive capture, offline export, Alt+number
switching, and mouse selection without touching a real cluster.

