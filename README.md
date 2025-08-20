[![Release](https://img.shields.io/github/v/release/JennyBr5/gopulse?label=Releases&color=007ec6)](https://github.com/JennyBr5/gopulse/releases)

# GoPulse — Real-Time Goroutine Tracing & Deadlock Detection

🫀 Live view of goroutine state, channel flow, and blocking stacks  
📈 Low overhead tracing, build-time or runtime attach  
🔍 Visualize channels, detect deadlocks, and profile concurrency

![GoPulse Live Trace](https://raw.githubusercontent.com/JennyBr5/gopulse/main/docs/trace-demo.gif)

Table of contents
- What GoPulse does
- Key features
- Concepts you need to know
- Quick start
- Command reference
- Instrumentation examples
- Visuals and UX
- Integrations and export
- Troubleshooting hints
- Contributing
- License

What GoPulse does
- Capture goroutine lifecycle events in real time.
- Map channel send/receive pairs and detect blocked operations.
- Surface deadlocks with stack traces and channel graphs.
- Provide an interactive timeline and flamegraph for concurrency hotspots.
- Run with negligible runtime overhead in production.

Key features
- Real-time tracing: stream goroutine and channel events to a UI.
- Deadlock detection: live alerts and backtraces when progress stalls.
- Channel visualization: show buffered/unbuffered channels and their interactions.
- Low overhead: sampling and event filters reduce cost.
- Attach modes: instrument at build time or attach at runtime.
- Export: capture traces in pprof-compatible and JSON formats.
- Integrations: works with standard library channels, net/http, context, and select patterns.

Concepts you need to know
- Goroutine: a lightweight thread managed by the Go runtime.
- Channel: a typed conduit for communication between goroutines.
- Blocked goroutine: a goroutine that waits on a channel send, receive, or a sync primitive.
- Deadlock: the scheduler stops forward progress because goroutines wait in a cycle.
- Trace event: a short record describing a goroutine state change or a channel op.
- Agent: the process that collects and ships events to the UI or file.

Quick start

1) Download and run the binary
- Visit the Releases page and download the appropriate release artifact.
- This file must be downloaded and executed: https://github.com/JennyBr5/gopulse/releases
- Example (Linux, replace with your release file): `./gopulse-linux-amd64`
- The binary runs a small UI and a collector endpoint on port 6060 by default.
- Use `--help` for runtime flags.

2) Attach to a running app (runtime attach)
- Start the agent: `./gopulse --mode agent --listen :6060`
- In your Go program, import the attach helper:
  - `import _ "github.com/JennyBr5/gopulse/attach"`
- Send a SIGUSR1 to the process or use the HTTP attach endpoint provided by the agent.
- The agent captures channel events and streams them to the UI.

3) Instrument at build time (compile-time)
- Add the GoPulse runtime package:
  - `import "github.com/JennyBr5/gopulse/runtime"`
- Initialize in `main()`:
  - `runtime.Start(runtime.Config{Addr: "127.0.0.1:6060"})`
- Build and run. The program will emit events to GoPulse.

4) Open the UI
- Point your browser to the agent UI: `http://localhost:6060`
- Or use the desktop mode: `./gopulse --open`

Command reference (common flags)
- `--mode` (agent/ui) — choose agent or standalone UI.
- `--listen` (host:port) — agent listen address.
- `--open` — open UI in default browser.
- `--filter` — filter events by package, goroutine id, or channel name.
- `--sample` — sampling rate for event capture (1 = all).
- `--export` — write captured trace to file in pprof or JSON.

Instrumentation examples
- Trace all sends on a channel named "work":
  - Tag a channel: `ch := gopulse.TagChannel(make(chan Work, 16), "work")`
  - GoPulse records tagged ops and surfaces them in the Channel view.
- Add manual spans around code sections:
  - `span := gopulse.StartSpan("db.query")`
  - `span.End()`
  - Spans appear in the timeline and group by goroutine.
- Use context-aware traces:
  - Attach trace IDs to contexts with `gopulse.WithTrace(ctx, "request-42")`
  - Trace flows across goroutines and network I/O.

Example: find a deadlock
- Start the agent and your app.
- If the system stalls, open the Deadlock tab.
- The UI shows blocked goroutines and the cycle that causes the deadlock.
- Click a goroutine to see a full stack trace and the channel edges.
- Use the suggested fix link to view code locations that need review.

Visuals and UX
- Timeline: scrollable, zoomable view of goroutine start/stop, block events, and spans.
- Channel graph: nodes for goroutines and channels; edges show send/receive links.
- Flamegraph: CPU and blocking flamegraphs for selected time windows.
- Live console: text feed of trace events with filters and search.
- Snapshot export: save a trace snapshot and load it later for offline analysis.

Screenshots
- Timeline view: https://raw.githubusercontent.com/JennyBr5/gopulse/main/docs/timeline.png
- Channel graph: https://raw.githubusercontent.com/JennyBr5/gopulse/main/docs/channel-graph.png
- Deadlock report: https://raw.githubusercontent.com/JennyBr5/gopulse/main/docs/deadlock.png

Integrations and export
- pprof: export blocking profile compatible with pprof tools.
- JSON: full event stream for custom processing.
- Grafana: send high-level metrics (goroutine count, blocked count).
- CI: fail a job if a run shows a deadlock or a sustained high blocked rate.

Performance and overhead
- GoPulse batches events and uses a non-blocking buffer.
- Sampling reduces overhead in hot paths.
- Instrument only packages you care about in high-throughput systems.
- Use `--sample` to dial cost vs. fidelity.

API snapshot
- StartSpan(name string) Span
- TagChannel(ch chan T, name string) chan T
- WithTrace(ctx context.Context, id string) context.Context
- StartAgent(addr string, opts ...Option) error
- ExportTrace(format string, path string) error

Troubleshooting hints
- If you see no events:
  - Ensure the agent address matches the runtime config.
  - Check firewall and container network rules.
- If the UI shows incomplete stacks:
  - Build with symbols enabled (do not strip binaries).
  - For runtime attach, ensure the process allows ptrace on your platform.
- If overhead feels high:
  - Increase sampling rate.
  - Limit package-level instrumentation.

Security considerations
- The agent exposes a network endpoint. Bind to localhost in production unless you secure it.
- Use firewall rules or TLS and API keys for remote collection.
- Avoid shipping traces with sensitive payloads. Use filters to strip data.

Examples and recipes
- Debugging HTTP handler leaks:
  - Tag request channels to follow request lifecycles.
  - Correlate goroutine start and end with query spans.
- Profiling worker pools:
  - Visualize channel queue length and worker goroutine states.
  - Use flamegraph to find blocking syscalls or heavy GC pauses.
- Finding a lost goroutine:
  - Use `StartSpan` around creation site and search for spans without end.

CI example
- Run unit tests under GoPulse and export a trace:
  - `./gopulse --mode agent --listen :6060 & go test ./... -run TestX && ./gopulse --export pprof test-trace.pprof`
- Store the trace artifact for post-mortem.

Release and download
- Stable binaries and packaged builds live on the Releases page.
- Download and execute the release binary file from: https://github.com/JennyBr5/gopulse/releases
- If the Releases link changes or does not respond, check the repository Releases section for the latest artifacts.

Contributing
- Open issues for bugs and feature requests.
- Fork the repo and send PRs against `main`.
- Keep changes small and test coverage high.
- Write tests for new runtime instrumentation and UI flows.
- Follow Go formatting and lint rules.

Roadmap highlights
- Native IDE plugins for VS Code and GoLand.
- Remote secure collectors with TLS and token auth.
- More export formats and CI-friendly checks.
- Better support for third-party channel wrappers and actor patterns.

Community and support
- Open an issue for feature requests or bugs.
- Submit a trace snapshot when reporting a hard-to-reproduce concurrency bug.
- Share anonymized traces to help reproduce issues.

License
- MIT License — see LICENSE file in the repository.

Topics
- channels, concurrency, deadlock-detection, debugging, go, golang, goroutines, monitoring, profiling, real-time, tracing, visualization

Credits
- Built by contributors who work on Go at scale.
- UX inspired by established tracing tools and pprof.

Quick links
- Releases: https://github.com/JennyBr5/gopulse/releases
- Repo: https://github.com/JennyBr5/gopulse
- Issues: https://github.com/JennyBr5/gopulse/issues

Maintainer
- JennyBr5 and the GoPulse contributors