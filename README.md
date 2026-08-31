# on

Run work on another machine, interactively.

```
on ol-agents claude
```

That starts `claude` on `ol-agents` — its CPU, its RAM, its checkouts, its
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
hosts:
  ol-agents:
    ssh: ol-agents              # an ssh_config alias — NOT a hostname
    workdir: ~/projects
    capabilities: [agent, ruby, node]
    projects: [offerlab]

  ik-agents:
    ssh: ik-agents
    capabilities: [agent, ruby, node, postgres, redis]
    projects: [influencekit]
```

`ssh:` names an **ssh_config alias**, never a hostname or IP. The alias already
carries the user, the identity file and any connection tuning — and it is the only
thing that distinguishes two accounts on one machine. A box reachable as both
`rex` (root) and `ol-agents` (olgm) is two different environments: different
`HOME`, `PATH`, toolchain and credentials. Keying on the hostname would make
"run it on that machine" ambiguous in a way that produces baffling failures.

It also keeps private addresses out of the file.

## Usage

```
on [flags] <host> <command>...   run in a tmux session there, and attach
on ls                            fleet health: cores, free memory, load
on ps                            live sessions across the fleet
on attach <host> [name]          reattach
on kill <host> <name>            end a session
on init                          write a starter inventory

flags (before the host):
  -C <dir>    remote working directory
  -n <name>   session name (default: derived from the command)
  -d          create but do not attach
  --new       always start a new session instead of reattaching
```

Flags come **before** the host so everything after it passes through untouched:
`on mona claude --resume` sends `--resume` to claude, not to `on`.

```
$ on ls
HOST           SSH                 CORES    AVAIL    TOTAL    LOAD
ik-agents      ik-agents               4   10464M   15615M    0.69  67% free
mona           mona                    4   11929M   15887M    0.05  75% free
ol-agents      ol-agents              16   24741M   31337M    0.28  78% free
```

## Behaviour worth knowing

**Re-running reattaches.** `on ol-agents claude` twice gets you back to the same
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
