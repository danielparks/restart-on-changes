# Restart a process when a path changes

```
restart-on-changes go run main.go
```

This restarts a process every time a file under a certain path is changed. It's
useful for workflows where a long running process needs to be

## Requirements

This only works on macOS.

## Options

### `-p PATH` and `--path PATH`

The path to watch for changes. Defaults to the current directory, i.e. `.`.

Any changes under that path will cause the process to restart.


