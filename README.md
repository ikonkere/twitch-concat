# vget

This is a fork of Arne Vogel's `concat` because i suck at golang and can't properly contribute but i use this tool daily and wanted to have features that are most useful to me.

![Example](https://i.ibb.co/3yZ6q8G/Screenshot-2020-04-09-at-12-20-24.png)

## Prerequisite

- OSX
- `brew install ffmpeg`
- increase max open files (`ulimit -n 4096`) to at least 4096 to avoid ffmpeg issues on large VODs

## Features

- removed Windows compatibility
- removed API key customization
- reworked concurrent download of chunks with worker pattern and channels
- implemented status updates from `ffmpeg`
- introduced progress bars for both chunk download and video encoding processes
