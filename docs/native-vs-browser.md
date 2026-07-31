# Why the app opens in a browser, and what that costs

`mm.exe` is not a GUI program. It is a local web server with the user interface
built into the binary, and the browser is only its screen. This is the same
architecture ComfyUI, SwarmUI and A1111 use, and it is a deliberate choice with
a real trade-off, so this page measures the trade-off rather than asserting it.

**Short version:** none of the work is done in the browser. Scanning, hashing,
downloading, verifying and indexing all run in native Go code inside `mm`. The
browser draws lists and buttons. Closing it does not stop anything.

## The measurements

Taken on the machine this was built on, against a 335 MB fixture of 40
safetensors files. Reproduce them on Windows with the commands below.

### Scanning is unaffected by the browser

`mm scan` over the fixture, twice with no browser process running at all, then
twice with the UI open in a real browser and a search rendered:

| Condition | Run 1 | Run 2 |
|---|---|---|
| No browser process | 0.748 s | 0.721 s |
| UI open in a browser | 0.770 s | 0.705 s |

Identical within run-to-run noise — roughly 465 MB/s of read-and-hash either
way. The scan never enters the browser, so there is nothing for the browser to
slow down.

### The daemon is tiny; the browser is the memory

| Process | Resident memory |
|---|---|
| `mm serve` (the whole application) | **4.9 MB** |
| Chromium showing the UI | **762 MB** across 12 processes |

That 155× gap is the honest cost of the browser, and it is worth understanding
precisely: it is the cost of *displaying* the app, not of running it. Close the
tab and it is all returned while every job keeps going. A native window would
replace that 762 MB with a smaller figure — a WebView2 shell is typically
50–150 MB — but it would not make the 4.9 MB of actual application any faster.

### Work survives the browser being killed

A download was started, then every browser process on the machine was killed
mid-flight. Result: the transfer completed, verified against its expected
SHA256, published into the model root, and indexed itself — with zero browser
processes alive and the daemon still answering on `/api/health`.

This is the clearest demonstration that the browser is a viewer. The job runs in
a goroutine inside `mm`; the page was only watching it.

### The UI's own latency is server-side

`/api/models` answers a 60-result search in **1.1–2.6 ms**. Search is SQLite
FTS5 inside the daemon. The browser's contribution is painting the results it is
handed.

## Reproduce it on Windows

```powershell
# 1. Scanning is unaffected. Run once with no browser open, once with it open.
Measure-Command { .\mm.exe scan --root D:\models --db .\perf.db }

# 2. Memory split: Task Manager -> Details, add the "Memory (private working
#    set)" column. Compare mm.exe against the sum of the msedge.exe entries.

# 3. Work survives the viewer. Start a download in the Browse tab, close the
#    window entirely, then reopen http://127.0.0.1:8737 -- the job is still
#    there, and the file lands whether or not you were watching.
```

## What a browser genuinely cannot do

These are real limits, not performance ones:

- No window of its own, no taskbar identity, no tray icon.
- No native file or folder pickers, and no "open containing folder" button.
- No drag-and-drop of files onto the app.
- No OS notifications when a long scan or download finishes.
- The memory overhead measured above.

Nothing on that list involves the GPU or disk. **No part of this application
uses the GPU** — hashing is disk-bound and CPU-bound, and runs in native code.
Any future feature that does need hardware belongs in the Go engine, where the
browser is not in the path at all.

## Why it is a server anyway

Because the server *is* the remote access. The same daemon that draws your
desktop window simultaneously serves:

- your phone, over a tailnet, as a PWA;
- any other machine on the network;
- third-party tools, via the documented HTTP API;
- a headless NAS with no display at all.

A native GUI rewrite would deliver a window and delete all four. That is the
whole trade, and it is why the browser stays.

## If a real window is wanted

The upgrade path that keeps everything above is a **WebView2 shell**: a small
native `.exe` that opens a proper window containing the same UI, talking to the
same server. It buys the window, tray icon, native pickers and lower memory,
costs a Windows-only build and a dependency, and changes nothing about the
engine or remote access.

Short of that, `start.bat` already opens the UI in an Edge/Chrome **app window**
— no tab strip, no address bar, its own taskbar entry. It looks like a desktop
application, uses the same browser engine underneath, and keeps the phone
working.
