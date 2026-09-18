# flushgopher ʕ◔ϖ◔ʔ

A Magento 2 cache cleaner and file watcher written in Go, based on
[mage-os/magento-cache-clean](https://github.com/mage-os/magento-cache-clean).
It uses the same rules for deciding what to clean, and a different engine for
watching files.

```
flushgopher -w              # watch and clean (run from the Magento root, or pass -d)
flushgopher                 # flush all caches
flushgopher config layout   # clean specific cache types
flushgopher -f changed.txt  # clean based on a list of changed files
```

## Why a rewrite

The original handles every file event on its own, and each cache type gets a
5s flood guard. A `git checkout` or `setup:upgrade` touches thousands of files,
so the original keeps re-cleaning the same cache types every 5 seconds for as
long as events keep arriving. It also shells out to `php` to read `env.php` and
the module list. That call fails whenever composer's autoloader is broken,
which is exactly what happens mid branch switch.

flushgopher works differently:

* **Batching.** Changes are collected until the file system has been quiet for
  300ms (`--debounce`). The batch is then cleaned once, using the union of all
  cache types and ids it needs.
* **Storm mode.** Once a batch grows past 40 files, or git is active, or the
  module list changes, flushgopher waits for 2s of quiet instead. It also holds
  off while `.git/index.lock` exists. A whole branch switch turns into a single
  clean.
* **Duplicate suppression.** A file whose size and mtime haven't changed since
  it was last handled is skipped. This covers FSEvents replays and editors that
  re-save without changing anything.
* **No PHP needed.** flushgopher parses `env.php` itself (a PHP array-literal
  parser; it only falls back to `php` for exotic files). It finds modules and
  themes by reading `vendor/composer/autoload_files.php` and each
  `registration.php`. For a store with around 800 modules this takes about
  20ms, where PHP takes about 300ms, and it still works when files are missing.
* **Persistent connections.** It keeps one redis connection open and sends
  pipelined commands, with no reconnect for each clean.
* **Cheap watching.** It runs a few FSEvents streams on top-level directories
  (`app`, `vendor`, ...) and filters the events by module and theme directory.
  macOS only.

## Hotkeys (watch mode)

```
caches   [c]onfig [b]lock_html [l]ayout [t]ranslate [f]ull_page [v]iew [m]isc [a]ll
files    [G]enerated code  [I]ntegration sandboxes  [F]rontend static [A]dminhtml static
gopher   [r]escan modules  [?] help  [q]uit
```

## Options

```
-d, --directory <dir>   Magento base directory (default: search upwards from cwd)
-w, --watch             Watch for file changes and clean affected caches
-f, --file-list <file>  Clean caches based on a list of changed files
-k, --keep-generated    Don't remove generated code / js-translation.json
    --debounce <ms>     Quiet time before a batch is processed (default 300)
    --dry-run           Only log what would be cleaned
-v / -vv / -s           More / much more / less output
```

`-n`/`--no-flood-guard` is still accepted for compatibility, but it does
nothing: batching replaces the flood guard.

## Supported backends

* File: `Cm_Cache_Backend_File`
* Redis/Valkey: `Cm_Cache_Backend_Redis` and Magento's Redis backend, over TCP,
  TLS or a unix socket, with AUTH/ACL
* L2 cache: `RemoteSynchronizedCache`
* Varnish: PURGE requests to `http_cache_hosts`
* Integration test sandboxes

## Build

```
make build     # bin/flushgopher
make install   # copies bin/flushgopher to ~/.local/bin
make test      # unit + integration tests (redis tests start their own redis-server)
```

To run it as `cl`:

```
echo "alias cl='$HOME/.local/bin/flushgopher -w'" >> ~/.zshrc && source ~/.zshrc
```

Building needs cgo, which is on by default on macOS, so the watcher can use
FSEvents.

## Comparing with the original

`test/compare/` has a harness that runs both tools against the same store. It
applies an identical sequence of edits to each and records every redis command
through `MONITOR`:

```
test/compare/compare.sh <magento> <redis.sock> <outdir> <name> <cmd...>
test/compare/analyze.py <outdir-orig> <outdir-go>
test/compare/storm.sh   <magento> <redis.sock> <outdir> <git-rev> <cmd...>
test/compare/storm_report.py <outdir>...
```

Both frontends in the store's `env.php` must point at the redis socket you
pass in. Use a throwaway redis and a git worktree, never your real caches.
