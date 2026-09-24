# on

Run work on another machine, interactively.

```
on builder claude
```

That starts `claude` on `builder` — its CPU, its RAM, its checkouts, its
credentials — inside a tmux session, and hands you the terminal. Detach and it
keeps running. Reattach from anywhere, including a phone.

## Why

A workstation running many agent sessions runs out of memory long before it runs
out of useful work. Each agent is a process plus its MCP servers, and the cost
scales with how many sessions exist rather than with what they are doing. Once the
machine is swapping, everything on it gets slower — including the sessions already
doing something useful, and any test suite you try to run alongside them.

Meanwhile there is usually idle hardware on the same tailnet.

`on` moves the session, not the state. There is no daemon, no protocol, and no
agent to install on the remote host. It builds an ssh command, starts a tmux
session, and attaches you to it.

## Install

```
go install github.com/bborn/on@latest
on init          # writes ~/.config/on/hosts.yaml
```

## Inventory

```yaml
# Clone URLs, used when a host is asked for a project it does not have yet.
repos:
  myapp: git@github.com:me/myapp.git

hosts:
  builder:
    ssh: builder              # an ssh_config alias — NOT a hostname
    workdir: ~/projects
    capabilities: [agent, ruby, node]
    repos:                    # project name -> checkout path on THIS host
      myapp: ~/projects/engineering

  testbox:
    ssh: testbox
    capabilities: [agent, ruby, node, postgres, redis]
    repos:
      myapp: ~/src/myapp
```

Note that `repos` maps a **project name to a path**, because directory names are
not project names — the same project is often checked out as `engineering` on one
host and `myapp` on another. Keying on the path would make placement guesswork.

`ssh:` names an **ssh_config alias**, never a hostname or IP. The alias already
carries the user, the identity file and any connection tuning — and it is the only
thing that distinguishes two accounts on one machine. A box reachable as both
`bigbox` (root) and `bigbox-dev` (an unprivileged account) is two different environments: different
`HOME`, `PATH`, toolchain and credentials. Keying on the hostname would make
"run it on that machine" ambiguous in a way that produces baffling failures.

It also keeps private addresses out of the file.

## Usage

```
on [flags] <host> <command>...   run in a tmux session there, and attach
on --repo <project> <command>... pick a host serving the project, and run there
on ls                            fleet health: cores, free memory, load
on ps                            live sessions across the fleet
on attach <host> [name]          reattach
on kill <host> <name>            end a session
on init                          write a starter inventory

flags (before the host):
  -C <dir>    remote working directory
  -r, --repo  project name; resolves to its checkout on the host
  -n <name>   session name (default: derived from the command)
  -d          create but do not attach
  --new       always start a new session instead of reattaching
```

Flags go before or just after the host; everything from the first non-flag
onward is the remote command, so `on devbox claude --resume` sends `--resume` to
claude rather than to `on`.

## Projects

```
on --repo myapp claude          # picks the host with the most free memory
on builder --repo myapp claude  # that project, on that host
```

If the chosen host has no checkout, `on` clones it from the top-level `repos:`
URL, so a host that has never seen the project behaves like one that has. When
several hosts serve a project, the one with the most memory available wins —
landing work where there is room for it is the entire point.

```
$ on ls
HOST           SSH                 CORES    AVAIL    TOTAL    LOAD
builder        builder                16   24741M   31337M    0.28  78% free
devbox         devbox                  4   11929M   15887M    0.05  75% free
testbox        testbox                 4   10464M   15615M    0.69  67% free
```

## Running a command against uncommitted work

```
cd ~/code/myapp
on exec bin/rails test
```

`on exec` rsyncs the current directory to a host, runs the command there, streams
output back, and exits with the remote command's status — so a failing suite
fails your shell exactly as a local run would. The project is detected from the
checkout's git remote, so usually no arguments are needed.

This is the case where the work is **uncommitted**: an agent has just edited
files and wants tests run somewhere with spare CPU. Committing first is not an
option, and a network filesystem is far too slow for a test suite.

What crosses is governed by `.gitignore`, including nested ones. A hand-written
exclude list cannot keep up with a working tree — the first real run of this
copied 623MB of compiled binaries and git history before the filter existed.
`.git` is skipped by default too. Native dependencies are deliberately left
behind: a macOS arm64 gem will not run on a Linux x86 host, so the remote builds
its own via a `setup` step:

```yaml
exec:
  myapp:
    setup: bundle install --quiet
    exclude: [storage/]
```

Keep `setup` cheap and idempotent — it runs on every invocation.

### Preparing a mirror

Some work is too slow to repeat per run but too important to leave out. Rails
assets are the case that motivated this: without a precompile, every test that
renders a layout raises `The asset "application.js" is not present in the asset
pipeline`. Minitest counts that as an **error**, not a failure, so the run still
prints `0 failures` and reads as a pass. On one real controller test that was 29
of 115 tests and 100 of 366 assertions, silently not running.

`prepare` runs once when a mirror is created, and again when the files it derives
from change:

```yaml
exec:
  myapp:
    setup: bundle install --quiet
    prepare: bin/rails assets:precompile
    prepare_inputs: [app/assets, app/javascript, package.json, yarn.lock]
```

Staleness is decided by modification time — `rsync -a` preserves them, so a file
you just edited arrives newer than the last prepare. **Omitting `prepare_inputs`
means the step runs once per mirror and never again**, which is right for a
one-off bootstrap and wrong for anything derived from source you are editing. A
failing `prepare` aborts the run rather than letting the command proceed without
what it needs.

### Files the sync leaves out

The sync skips anything gitignored, which includes the local config an app needs
to boot for real, such as `config/application.yml`. List those per project and
ask for them with `--include`:

```yaml
exec:
  myapp:
    include: [config/application.yml, config/master.key, .env]
```

```
on exec --include cloud bin/dev       # a dev server that needs the real config
```

Each file is read from the tree you are standing in, following a symlink to what
it points at. A file the tree lacks comes from the repository's main checkout,
the same place worktree setup scripts link them from. Files land `0600`.

Runs without `--include` delete them from the mirror first. Test runs then see
exactly what CI sees, not keys that happened to be left behind by a dev server.

### Environment and serialisation

```yaml
exec:
  myapp:
    env:
      PARALLEL_WORKERS: "1"
    lock: myapp
```

`env` is exported before setup, prepare and the command, so a project states its
remote environment once instead of every caller remembering to prefix it.

`lock` serialises runs sharing that name on a host. Mirrors are isolated from
each other; the things they talk to are not. Two concurrent `on exec` test runs
against one host share a test database, and above Rails' 50-test parallelisation
threshold they deadlock in Postgres — walls of `PG::TRDeadlockDetected` with zero
assertion failures, which reads as a regression that does not exist. The lock is
opt-in and named so cheap commands are not made to queue behind test suites, and
it is held for exactly as long as the command runs. A host without `flock` says
so on stderr and runs unserialised rather than pretending.

Point an agent at it from `CLAUDE.md` or `AGENTS.md`:

> Run tests with `on exec bin/rails test`, never `bin/rails test`.

## Elastic pools

Fixed hosts are the cheap default. When none of them has room, `on exec` can boot
a server from a snapshot, run there, and let it be deleted once it sits idle.

A pool says how big a server it needs, not which one. Every provider it lists
reports what it could boot: each type, in each location, that can boot the pool's
image. `on` converts the prices to one currency and tries them cheapest first. A
sold-out type fails fast and the next cheapest is tried, so a stock-out on one
provider falls through to another instead of failing the run.

```yaml
elastic:
  cloud:
    image: myapp               # boots the newest snapshot labelled (tagged) on-image=myapp
    build: ~/images/myapp.sh   # provisions a fresh server into the image; see below
    min_cpus: 8                # the smallest server the pool will boot
    min_memory_gb: 30
    currency: EUR              # of daily_budget and the ledger (default EUR)
    rates: {USD: 0.86}         # converts other providers' prices; value of 1 USD in EUR
    providers:
      hetzner:
        context: myproject     # hcloud context, so the token never lives in this file
        locations: [fsn1, nbg1]
        ssh_keys: [laptop, reaper]
      digitalocean:
        token_file: ~/.config/on/digitalocean.env   # DIGITALOCEAN_ACCESS_TOKEN=…
        locations: [nyc3, sfo3]
        ssh_keys: [laptop, reaper]
        # types: [s-8vcpu-32gb]  # optional: only these, instead of any big enough
    user: dev                  # the login baked into the image
    serves: [myapp]
    min_free_mb: 8000          # below this on every fixed host, use the pool
    idle_minutes: 20           # `on reap` deletes a server unused this long
    max_hours: 12              # and any server this old, busy or not
    max_servers: 2             # across providers
    daily_budget: 5            # per UTC day, in currency
```

Equal prices go to the location listed first. Hetzner is driven through the
hcloud CLI; DigitalOcean through its API, with the token read from `token_file`
(or `DIGITALOCEAN_ACCESS_TOKEN`). A pool written before providers existed
(`provider: hetzner` with `types`, `locations` and `ssh_keys` at the top level)
still loads.

```
on offers cloud                   # what the next server would be, cheapest first
on exec cloud bin/rails test      # force the pool
on up cloud                       # start a server now, e.g. for a dev server
on forward cloud 3000             # http://localhost:3000 is the server's port 3000
on pools                          # servers, providers, prices, today's spend
on down on-cloud-3fa2c1           # delete now
on reap                           # run every few minutes on an always-on machine
on image build cloud digitalocean # build the pool's image on a provider
```

**Images are built per provider.** A snapshot cannot move between clouds, so each
provider keeps its own. `on image build <pool> <provider>` boots the cheapest
plain Ubuntu 24.04 server of at least 4 CPU / 8 GB there, runs the pool's `build`
script with the server's IP as its argument (it logs in as root), snapshots the
result, deletes older snapshots, and deletes the server, even if the build
fails (`--keep` leaves it for debugging). The builder is kept small because its
disk becomes the image's minimum disk. On DigitalOcean the snapshot is also
copied to the pool's other regions, since a droplet can only boot from a
snapshot in its own region; until a copy finishes, that region is not offered.

**Servers resolve as ssh aliases.** `on` writes one `Host` block per server to
`~/.config/on/ssh_config.d/<pool>.conf`; add
`Include ~/.config/on/ssh_config.d/*.conf` near the top of `~/.ssh/config`.

**Deleted, never stopped.** Both providers bill a powered-off server, so idle
servers are deleted. `on reap` counts a server as busy while a tmux session exists
or any process has its working directory under the workdir (where `on exec`
mirrors and `on forward` keepalives live — a keepalive ends with its tunnel),
which protects it from the idle rule but not from `max_hours` or the budget.

**The budget is enforced in two places.** `on reap` estimates spend per started
hour in a ledger (`~/.local/state/on/ledger.json`); for a provider that bills by
the second that overstates, which is the safe side for a cap. Once a pool reaches
its `daily_budget` it deletes the pool's servers and marks every provider's image
`on-paused=<day>`, which stops `on exec` on every machine from starting another
until the next UTC day. Without a machine running `on reap` on a schedule, idle
servers are never deleted, so set that up first.

**Everything is labelled.** Servers carry `on=elastic` and `on-pool=<pool>`
(DigitalOcean tags `on:elastic` and `on-pool:<pool>`), and `on` only ever lists or
deletes servers with both, so a pool can share an account with servers `on` did
not create. A provider that cannot be reached is reported and skipped, so it
cannot hide the others' servers from `on reap`.

## Knowing where you are

With `--repo` the host is chosen for you, so `on` prints the target before
attaching and labels the tmux status bar:

```
→ builder  ~/projects/engineering
[builder] on-myapp-claude
```

The label is the inventory name, not tmux's `#H`: one machine reached as two
users is two environments, and `#H` reports the same hostname for both.

`on ps` lists every session with its host.

## Behaviour worth knowing

**Re-running reattaches.** `on builder claude` twice gets you back to the same
session rather than starting a second one, because that is almost always what you
meant. Use `--new` for a genuinely separate session.

**Disconnection does not kill work.** The tmux session lives on the remote host.
Close the lid, lose the wifi, reattach later — the agent kept going. This is the
main thing a local session cannot do.

**Unreachable hosts fail loudly.** There is no automatic fallback to running
locally. A silent fallback would quietly reintroduce the memory pressure the tool
exists to remove, on exactly the days you are least likely to notice.

**Nothing here moves secrets.** `on` resolves a host and runs a command.
Credentials are a property of the host, provisioned out of band. Nothing is copied,
forwarded, or written to disk by this tool.

## Requirements

The remote host needs `tmux`, `git` and whatever you intend to run. Hosts are
expected to be provisioned in advance — `capabilities` and `projects` in the
inventory describe what a host is already set up for, they do not install
anything.

Let the repository's own `mise.toml` or `.ruby-version` drive runtime versions
rather than relying on the host default, or version differences between hosts will
present as code bugs.

## License

MIT
