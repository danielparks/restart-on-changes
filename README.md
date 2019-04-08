# Restart a process when a path changes

```
restart-on-changes -x .git go run main.go
```

This restarts a process every time a file under a certain path is changed. It's
useful for workflows where a long running process needs to be

## Requirements

This only works on macOS.

## Options

### `-x GLOB` and `--exclude GLOB`

Patterns to match files and directories to exclude.

* `-x .git` match `.git` the current directory.
* `-x '**/.DS_store'` matches `.DS_store` in the current directory and any
  directories below it.

You may include this option multiple times to exclude multiple patterns. For example:

    restart-on-changes -x .git -x '**/*.o' -x myapp sh -c 'make && build/myapp'

### `-p PATH` and `--path PATH`

The path to watch for changes. Defaults to the current directory, i.e. `.`.

Any changes under that path (except exclusions) will cause the process to
restart.
