---
title: Primitives Reference
description: A deeper, per-concept reference for the nine building blocks of Gas City — Session, Task Store, Event Bus, Config, Prompt Templates, Messaging, Formulas, Dispatch, and Health Patrol — each with a copy-pasteable example.
---

The [Architecture Overview](/concepts/architecture-overview) gives you the
top-down mental model. This page is the bottom-up companion: a reference you
can dip into for any single building block once you know where it sits in the
whole.

Gas City is built from **nine concepts** — five irreducible *primitives* and
four *derived mechanisms* composed from them. Everything `gc` does, from
slinging a single task to running a fleet of pooled agents, is some
combination of these nine. Read this page after the overview, or jump straight
to the concept you need.

<Note>
This is reference material, not a tutorial. Each section explains what a
concept is, what it does for you, and shows one snippet you can copy-paste.
For the guided, end-to-end path, start with the
[Tutorials](/tutorials/index).
</Note>

## The five primitives

A **primitive** is irreducible: you cannot rebuild it out of the other
concepts, and removing it would make whole classes of orchestration
impossible. There are exactly five.

### Session

A **session** is a single running instance of an agent — a live process (by
default a `tmux` pane) that Gas City can start, stop, prompt, and observe,
regardless of which provider backs it.

Sessions are deliberately **disposable**. They come and go; the work they were
doing survives them, because work lives in the [Task Store](#task-store-beads),
not in the process. This is what lets the controller restart a stalled agent,
replace a crashed one, or **adopt** a still-running one after the controller
itself restarts — without losing anything.

A session's *identity* is stable even though the process is not. The same
agent always resolves to the same session name, so the controller can find,
re-attach to, or replace it across restarts.

**What it does for you:** you never manage processes by hand. You declare
agents in [config](#config); the controller spawns, supervises, and reaps
sessions to match. When you need to look inside one, you address it by name.

```shell
# List the live sessions in your city
gc session list

# Peek at what an agent is currently doing
gc session peek mayor --lines 20

# Attach to a session interactively (detach with the provider's keybinding)
gc session attach mayor
```

> Under the hood, the runtime boundary is a `Provider` interface with
> implementations for tmux (production), subprocess (remote), exec (script),
> Kubernetes, and a fake provider for tests. They all expose the same
> start/stop/prompt/observe surface, so nothing above the session layer cares
> which one is in use.

### Task Store (Beads)

The **Task Store** — almost always just called **beads** — is the universal
persistence substrate. The rule is absolute: *everything is a bead*. A bead is
one row in one store, and tasks, mail messages, molecules, convoys, and epics
are all beads that differ only by their `type` field.

A **bead** is a single unit of work with an ID, a title, a status (`open` →
`in_progress` → `closed`), a type, an optional assignee, parent/child links,
dependencies (`needs`), a description, and labels. The store offers one small
interface over all of them: create, read, update, close, list, query by label,
and walk parent/child relationships.

**What it does for you:** it is the single source of truth for *what work
exists and what state it's in*. Because every piece of durable state flows
through this one interface, the system converges to correct outcomes even as
sessions churn — kill every agent and the work is still there, waiting to be
picked up again.

By default the store is backed by Dolt through the `bd` CLI, with one Dolt
server per city. The city and its rigs share that server but stay logically
separate by `issue_prefix`. See
[Beads Storage Topology](/internals/beads-topology) for where the files live.

```shell
# Create a task bead
bd create --title "Add a health endpoint" --type task --priority 2

# Find work that is ready (open, unblocked) and inspect one bead
bd ready
bd show <bead-id>

# Claim it, then close it when done
bd update <bead-id> --claim
bd close <bead-id> --reason "Shipped in v1.1.0"
```

<Tip>
A **label** is just a string tag on a bead (e.g. `pool:dog`,
`rig:tower-of-hanoi`). Labels drive pool dispatch and rig scoping, and you can
query by them with `bd list --label <name>`. They are how higher-level
mechanisms route and group work without any new storage.
</Tip>

### Event Bus

The **event bus** is the universal observation substrate: an append-only
pub/sub log of everything that happens in the system. Events are immutable and
carry a monotonically increasing sequence number, so observers can replay from
any point and never miss or reorder an event.

It has two tiers:

- **critical** events on a bounded queue, for infrastructure that must not drop
  anything;
- **optional**, fire-and-forget events for audit and visibility.

Other parts of the system *watch* the bus reactively instead of polling it,
which is what keeps the controller responsive without busy-looping.

**What it does for you:** it is how you (and the rest of the system) see what
is happening as it happens — a bead being created, a convoy closing, a session
restarting. It is the backbone behind every `--watch`/`--follow` view.

```shell
# Show recent events
gc events

# Filter by type and time window
gc events --type bead.created --since 1h

# Follow new events live (Ctrl-C to stop)
gc events --follow --type convoy.closed
```

### Config

**Config** is TOML with *progressive activation*: capabilities switch on simply
because a section is present, not because you flipped a feature flag. An empty
`city.toml` gives you the bare minimum; adding sections unlocks more.

`city.toml` is the single deployment config file. It declares which agents
should exist, how pools scale, where mail and formulas live, and so on. The
[Architecture Overview](/concepts/architecture-overview#controller) explains
why this file *is* the desired state the controller reconciles toward — there
is no separate state file to maintain.

**What it does for you:** it is the one place you describe what your city
*should* look like. Change it and the controller notices (it watches the file)
and drives reality to match — no restart required for most changes.

```toml
# city.toml — a minimal two-agent city
[workspace]
name = "bright-lights"

[[agent]]
name = "mayor"
provider = "claude"

[[agent]]
name = "worker"
provider = "claude"
```

The capability levels activate like this — you never set a "level" directly,
it is implied by which sections you have written:

| Level | What you add | What it unlocks |
| ----- | ------------ | --------------- |
| 0–1   | `[workspace]` + `[[agent]]` | Sessions + tasks |
| 2     | `[daemon]` | The reconciling task loop (controller) |
| 3     | `[agent.pool]` | Multiple agents + elastic pools |
| 4     | `[mail]` | Messaging |
| 5     | Formula files | Formulas & molecules |
| 6     | `[daemon]` health fields | Health monitoring |
| 7     | `orders/` directories | Orders |
| 8     | All of the above | Full orchestration |

### Prompt Templates

A **prompt template** is a Go `text/template` written in Markdown that defines
*what an agent does*. It is the entire behavioral specification for a session —
the SDK contains **zero** hardcoded roles, so a "mayor" or a "reviewer" is
nothing more than the prompt you wrote for it.

Templates are rendered at spawn time with context about the city, the agent,
the rig, and git metadata. That rendered text is handed to the session as its
priming prompt.

**What it does for you:** this is where you express *intent*. Instead of
encoding role logic in code (which Gas City forbids — see
[ZFC](#a-note-on-design-principles)), you write a sentence and let the model
act on it. Want a different role? Write a different prompt.

```markdown
<!-- agents/reviewer/prompt.template.md -->
# Reviewer

You are the reviewer for **{{ .RigName }}** (working in `{{ .WorkDir }}`).

Check your hook for assigned work, review the change, and leave findings.
Find your pool work with: `{{ .WorkQuery }}`

When you are done, close the bead with a one-line summary.
```

Common template variables include `{{ .AgentName }}`, `{{ .RigName }}`,
`{{ .RigRoot }}`, `{{ .WorkDir }}`, `{{ .WorkQuery }}`, `{{ .IssuePrefix }}`,
`{{ .CityRoot }}`, and `{{ .DefaultBranch }}`. Prompt file discovery prefers
`prompt.template.md` (with `prompt.md` and `prompt.md.tmpl` accepted for
compatibility).

## The four derived mechanisms

A **derived mechanism** is one that is *composed* from the primitives above —
it needs no new storage, no new runtime, no new infrastructure. Each one below
is just a particular combination of Session, Task Store, Event Bus, and Config.

### Messaging

**Messaging** is how agents talk to each other. It is two things, neither of
which is a new primitive:

- **Mail** is a bead with `type: message`. An agent's inbox is a query for
  open message beads addressed to it; archiving a message is closing that bead.
  Mail is therefore *just the Task Store*.
- **Nudge** is text typed directly into a running agent's session to prod it.
  It is fire-and-forget and uses *just the Session* layer.

**What it does for you:** durable, queryable inter-agent communication (mail)
plus a lightweight "wake up and re-check" poke (nudge) — without learning any
new concept. Mail persists and survives restarts; a nudge does not.

```shell
# Mail: durable, shows up in the recipient's inbox
gc mail send mayor -s "Review needed" -m "Please look at the auth changes"
gc mail inbox mayor

# Nudge: ephemeral, prods a live session to act now
gc session nudge mayor "Check mail and hook status, then act accordingly"
```

### Formulas & Molecules

A **formula** is a reusable, multi-step workflow written as TOML. A
**molecule** is a formula *instantiated at runtime*: one root bead plus child
step beads in the Task Store, with progress tracked by closing those beads. A
**wisp** is an ephemeral molecule that auto-closes and is garbage-collected
after a TTL.

The derivation is pure composition: [Config](#config) parses the formula file,
and the [Task Store](#task-store-beads) holds the root and step beads. Steps
declare dependencies on each other with `needs`, so the store's readiness
queries naturally schedule them in the right order.

**What it does for you:** instead of slinging work one piece at a time, you
describe a whole workflow once and dispatch it as a unit. The steps fan out and
join automatically based on their dependencies.

```toml
# formulas/pancakes.toml
formula = "pancakes"
description = "Make pancakes from scratch"

[[steps]]
id = "dry"
title = "Mix dry ingredients"
description = "Combine flour, sugar, baking powder, salt in a large bowl."

[[steps]]
id = "wet"
title = "Mix wet ingredients"
description = "Whisk eggs, milk, and melted butter together."

[[steps]]
id = "combine"
title = "Combine wet and dry"
description = "Fold wet into dry. Do not overmix."
needs = ["dry", "wet"]
```

```shell
# See available formulas, then dispatch one as a molecule
gc formula list
gc sling worker pancakes --formula
```

Here `dry` and `wet` have no dependencies and can run in parallel; `combine`
waits for both. See [Tutorial 05](/tutorials/05-formulas) for the full walkthrough.

### Dispatch (Sling)

**Dispatch** — invoked with `gc sling` — is the routing mechanism that turns
"do this work" into a running agent. It composes the primitives end to end:
find or spawn an agent (Session), select a formula if one applies (Config),
create the work bead or molecule (Task Store), hook it to the agent, nudge the
session, optionally create a convoy to group related work (Task Store), and log
an event (Event Bus).

**What it does for you:** it is the single command that gets work *moving*.
Sling a plain description for a one-off task, or sling a formula to kick off a
whole molecule. Either way the work lands in the store, gets routed, and a
session picks it up on the controller's next tick.

```shell
# Sling a single task to an agent
gc sling claude "Create a script that prints hello world"

# Sling a formula — expands into a multi-step molecule
gc sling worker pancakes --formula
```

<Tip>
A **convoy** is a container bead that groups related work as one tracked batch;
child beads link to it via their parent. Dispatch can create a convoy for you
so a fan-out of related tasks reports progress as a unit.
</Tip>

### Health Patrol

**Health patrol** keeps the fleet alive. It probes sessions for liveness
(Session), compares what it finds against thresholds (Config), publishes stalls
to the [event bus](#event-bus), and restarts unhealthy sessions with backoff.
The supervision model follows the Erlang/OTP "let it crash, then restart"
pattern.

Crucially, the **controller** drives all of this on its own — no
user-configured agent role is required for the infrastructure to stay healthy.
If removing an `[[agent]]` entry would break supervision, that would be a bug.

**What it does for you:** stalled and crashed sessions recover automatically.
You declare the health thresholds in config; the controller does the probing,
restarting, and backoff. When you want to check the system's health yourself:

```shell
# Check workspace health (add --fix to attempt automatic recovery)
gc doctor

# Check the beads provider specifically
gc beads health
```

## A note on design principles

These nine concepts are not an arbitrary list — they are the *minimal* set that
makes multi-agent orchestration possible. Three rules keep the boundary honest:

- **Atomicity.** If a capability can be decomposed into the five primitives,
  it is a derived mechanism, not a new primitive. That is why Messaging,
  Formulas, Dispatch, and Health Patrol are *composed*, not built.
- **Bitter Lesson.** Every primitive must become *more* useful as models
  improve, never less. Gas City adds no heuristics or decision trees that a
  better model would outgrow.
- **ZFC (Zero Framework Cognition).** Go handles transport, not reasoning. If a
  line of Go contains a judgment call, it is a violation — the decision belongs
  in a [prompt template](#prompt-templates), not in code.

This is why all role behavior is configuration and the SDK has *zero* hardcoded
roles: the model is the intelligence, and these nine concepts are only the
plumbing it acts through.

## Where to go next

- [Architecture Overview](/concepts/architecture-overview) — the top-down view
  these primitives compose into.
- [Tutorials](/tutorials/index) — the guided, end-to-end path through every
  concept above.
- [Tutorial 06: Beads](/tutorials/06-beads) — go deeper on the Task Store that
  underpins everything here.
- [Beads Storage Topology](/internals/beads-topology) — how a city and its rigs
  share one store under the hood.
- [Reference](/reference/index) — command, config, formula, and provider lookup.
