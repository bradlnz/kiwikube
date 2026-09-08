<p align="center">
  <img src="assets/kiwikube-captain.png" alt="A purple kiwi holding a ship's wheel" width="160">
</p>

<h1 align="center">KiwiKube</h1>

<p align="center"><strong>Take the helm of your Kubernetes clusters.</strong></p>
<p align="center">A Linux terminal dashboard with live resources, searchable history, and shells at your fingertips.</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-a3e635?style=flat-square" alt="License: MIT"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.27-22d3ee?style=flat-square&amp;logo=go&amp;logoColor=white" alt="Go 1.27"></a>
  <a href="#contributing"><img src="https://img.shields.io/badge/contributions-welcome-a78bfa?style=flat-square" alt="Contributions welcome"></a>
</p>

<p align="center">
  <a href="#screenshots">Screenshots</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="docs/guide.md">Guide</a> ·
  <a href="#contributing">Contributing</a>
</p>

## Screenshots

**Node stats** — CPU, memory, disk, network, and node details.

![Node stats and details](screenshots/node-stats.png)

<details>
<summary>Pod management, error search, shells, and routing</summary>

**Pod management** — readiness, restarts, containers, and metrics (`m`).

![Pod management and metrics](screenshots/pod-metrics.png)

**Find errors** — open pod logs, then Ctrl+F → `ERROR` → Enter.

![Live pod logs filtered to ERROR](screenshots/pod-log-search.png)

**Shell access** — `s` opens a pod shell; `exit` or Ctrl+] returns. `S` opens node SSH.

![Interactive pod shell](screenshots/pod-shell.png)

**Routing** — Ingress → Service → Pod (`F`).

![Routing graph](screenshots/flow.png)

</details>

*Demo data. Network gauges scale to the observed peak.*

## Quick start

Build: Go 1.27, a C compiler, pkg-config, and libvterm 0.3+ headers.
Run: Linux, `kubectl`, `stty`, and SQLite 3.38+ with FTS5. SSH is optional.
[Package details](docs/guide.md#requirements).

```sh
git clone https://github.com/bradlnz/kiwikube.git
cd kiwikube
./build.sh
./kiwikube
```

Uses your kubeconfig. Switch clusters with **Alt+1–5**; press **?** for help.
Local history records by default.

[Controls, themes, history, and performance →](docs/guide.md)

## Contributing

[Issues](https://github.com/bradlnz/kiwikube/issues) and pull requests welcome.
[Development checks](docs/guide.md#checks) · [MIT License](LICENSE).
