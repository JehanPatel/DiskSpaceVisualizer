# Disk Space Visualizer

A lightweight Go desktop-style utility that starts a private local web UI, scans a folder or drive, and shows where storage is going with a treemap.

## Features

- Treemap visualization of folders and large files
- Top file list with size, modified date, and folder
- Huge old file filter
- Reveal files in Explorer
- Move selected files to the Recycle Bin
- Embedded UI, no external runtime required after building

## Run

```powershell
go run .
```

## Build a standalone Windows exe

```powershell
go build -ldflags="-s -w" -o diskspace-visualizer.exe .
```

The executable embeds `web/index.html` and serves it from `127.0.0.1` on a random port with a one-time token.
