# Instructions for coding agents

multi-ping is a terminal app that pings destinations through several network
interfaces at once and compares their delay and loss. Go, Bubble Tea, no cgo.
Read `README.md` first for what the user sees.

## Layout

| Path | Contents |
|---|---|
| `main.go` | flags, plain mode, program start |
| `tui.go` | the Bubble Tea model: state, keys, statistics, rendering |
| `log.go` | probe records, CSV / JSON Lines log, the dump |
| `internal/ping/ping.go` | interface list, name resolution, `Ping` dispatcher (all OSes) |
| `internal/ping/ping_unix.go` | ICMP sockets for Linux and macOS |
| `internal/ping/bind_*.go` | how a socket is pinned to an interface, per OS |
| `internal/ping/ping_windows.go` | `IcmpSendEcho2Ex` / `Icmp6SendEcho2` |
| `internal/ping/gateway_*.go` | default-gateway lookup, per OS |
| `docs/` | README screenshots and the script that takes them |

## Commands

```
make build     # ./multi-ping
make test      # go vet + go test
make dist      # empty dist/, cross-compile all six targets into it, write SHA256SUMS
```

Before finishing any change, all of these must pass:

```
gofmt -l .                      # prints nothing
make test
GOOS=windows go vet ./...
GOOS=darwin go vet ./...
```

## Rules

- **Three platforms.** Linux, macOS and Windows must keep compiling without
  cgo. OS-specific code goes in a `_linux.go`, `_darwin.go` or `_windows.go`
  file (or a `//go:build` tag) inside `internal/ping`; nothing outside that
  package may depend on the OS.
- **No privileges.** The app must work without root or administrator rights.
  Do not switch to raw sockets or shell out to the system `ping`.
- **Say what was not run.** Only the code for the OS you are on can be
  executed. When you change another OS's backend, state in your summary that
  it compiles but was not run.
- **Tests.** Behaviour changes in `tui.go` and `log.go` need a test in
  `tui_test.go`. Tests must not open sockets or need a network; drive the
  model with messages instead (see `TestInterfaceRefresh`).
- **Bubble Tea discipline.** `Update` must not block: network and file-system
  waits belong in a `tea.Cmd`. Results come back as messages carrying the
  panel pointer and its `epoch`, so replies that arrive after a reset are
  dropped.
- **Every rendered line is clipped** to its width (`clip`, `wrap`); the view
  must never exceed the terminal. `TestViewFitsTerminal` checks this.
- **Record format.** The JSON Lines schema in `log.go` is documented in the
  README and has a `format` number. Adding a field is fine; renaming or
  removing one requires bumping `format` and updating the README.
- **README.** Keep flags, keys and the record format in the README in step
  with the code. Retake the screenshots with `docs/screenshot.py` when the
  layout changes visibly.
- **Dependencies.** Do not add modules without a clear need.

## Commits and branches

- Do not mention any AI agent, assistant or coding tool in commit messages,
  branch names, pull-request titles or descriptions.
- Do not add `Co-Authored-By`, session links or similar trailers for an agent.
- Write commit subjects in the imperative, describing the change itself.
