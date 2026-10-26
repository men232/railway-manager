# railway-manager design

This document explains how railway-manager works and why it is built the way it is. It describes the system as it stands; the code under `internal/` is the final word where the two ever disagree.

---

## 1. Intent

railway-manager is a terminal app for a small team that runs several Railway projects. It covers variables, secrets, database passwords, service limits, volumes, domains, restarts, deployments, logs and change history.

Its core is a **change set**: a local list of uncommitted changes to Railway. Any screen can add to it. The user reviews it in one place and applies it when ready. Applying compiles the change set into a dependency graph of idempotent steps, journals every step to disk, and resumes from the step that failed.

The architecture comes first. Adding a feature (a new setting, a new action) should mean adding one *kind* of change, without touching the change-set core, the compiler or the executor.

### Goals

- One local change set per user, fed by every screen: Services, Variables, Rotate secrets.
- Applying builds a DAG of steps. Each step checks before it acts, its status is journaled, and a re-run continues from the failed or unfinished steps.
- An immediate action, such as restarting one service now, is a one-change change set applied at once through the same engine.
- Every screen shows Railway's data with the change set laid over it: what a target is now and what it will be.
- Live data: deployment status over a websocket plus polling, metrics, logs.
- Installed with `go install`. Each user has their own `railway login` and their own change set.

### Non-goals

- Shared or team-visible change sets. Railway's own pending-changes feature is not used.
- Rollback of deployments and replica counts. The kind registry is meant to make these easy to add later.
- A web UI, a daemon, or a scripting interface beyond a few CLI flags.
- Zero-downtime database password changes. New connections fail from the moment the password changes inside the database until each app's new deployment is live. No dual-password scheme is used.
- Database clients written in Go. `ssh`, `mongosh`, `psql` and `redis-cli` stay external programs.

### Failures even if it "works"

- A secret printed to the terminal, a log, a golden file, the journal or an error message. The only exception is the explicit reveal key.
- A secret in a subprocess argv or in a URL on a command line.
- A service redeployed with a half-applied value.
- A re-run of apply that writes something twice in a way that is not idempotent, or that skips a step whose effect is not actually in Railway.
- A screen that fetches from the network itself. Screens read the store; loaders write it.
- A live test that touches anything outside the environment it was explicitly pointed at.

### Constraints

- Go, Bubble Tea, Bubbles and Lip Gloss. No cgo, so `go install` works anywhere.
- macOS and Linux. Locks and quarantine rely on hard links, so the work dir needs a filesystem that supports them.
- The `railway` CLI is needed for token refresh and for `railway ssh config --dry-run`.

---

## 2. Concepts

| Term | Meaning |
|---|---|
| **Place** | Where a change lands: `project/env/service`, or `project/env/shared` for shared variables. |
| **Target** | What changes at a place: a variable by name, the CPU/memory limits, a volume, a domain, a database user, or the service itself for actions (restart, redeploy). |
| **Change** | One intent: its kind, place, target, the new value or action, and the *base*, meaning what the target was when the change was made. |
| **Change set** | The local, ordered list of changes not yet applied. One per user, always present, possibly empty. |
| **Kind** | A type of change with its own validation, display, drift check and steps. |
| **Step** | One idempotent unit of work in Railway or in a database: patch an environment, change a password inside a database, redeploy a service, verify. |
| **Run** | One application of a selection of the change set: the frozen changes, the compiled DAG and a journal of step statuses. A failed run can be resumed. |
| **Effective view** | Railway's data with the change set overlaid. This is what screens show. |

---

## 3. Architecture

### 3.1 Layers

```
cmd/railway-manager     entry point
internal/
  app/                  flags, work dir, wiring of everything below
  railway/              GraphQL client, websocket subscriptions, token source and refresh
  store/                the in-memory data model, revisions, indexes, load status
  loaders/              tea.Cmds that read Railway and return messages for the store
  view/                 selectors: pure, memoized functions from store + change set to screen rows
  changeset/            Change, ChangeSet, persistence, CheckConflicts
  kinds/                one file per kind: variable, limits, volume, domain, deploy, db password, keyfile, keep
  apply/                compiler (change set → DAG), executor, journal, runs, locks
  rotation/             grouping, fingerprints, classification, generation, password rows, URL rewrite
  dbops/                ssh tunnels and the mongosh/psql/redis-cli clients
  proc/                 process runner (real and fake)
  domain/               places, fingerprints, reference parsing
  monitor/  settings/  workdir/  text/  version/
  tui/                  root model, screen stack, frame, footer, theme, icons, components
  tui/screens/<name>/   one package per screen
```

Dependencies point one way: `tui` → `view`, `changeset`, `apply`, `loaders` → `store`, `railway`. `store`, `changeset`, `rotation` and the pure parts of the kinds never import `tui`. Everything that touches the network, a process, the clock or the disk sits behind an interface, so tests replace it.

### 3.2 Store and the effective view

There is no database. In Bubble Tea, state changes only in `Update`, on one goroutine, and `View` renders from the current state, so the app needs no reactive data layer. Joins are a few lines over indexes.

- **Store.** Typed Go structs per collection: catalog (projects, environments, services, instances), variables (unrendered, rendered, `updatedAt`, sealed, literal, fingerprint), references (forward and reverse edges), environment configs, Railway system values (`RAILWAY_PRIVATE_DOMAIN`, `RAILWAY_TCP_PROXY_*`, kept apart from variables), deployment states, metrics, volumes, domains, history entries and members.
- **Only `Update` writes it.** Loaders run in the background and return a message carrying data. The root model applies it to the store. Nothing else writes the store, so it needs no locks. Snapshots handed to background work are clones.
- **Revisions.** Each collection has a revision counter, bumped on every write. The change set has one too (`Rev`).
- **Selectors** (`view/`) turn store and change set into rows: variables by name or by value, places of a name, rotation rows, reference chains, the services table, the change-set diff, stalled runs. Each memoizes on the revisions it reads and its arguments, so a render recomputes nothing that did not change.
- **Overlay.** A selector that shows a target also says whether the change set touches it and what it becomes (`2 → 4 vCPU`, `→ 64 chars`, `✗ delete`, `+ added`). The kind formats this, so the overlay logic lives in one place.
- **Load status.** Each load has a key (`catalog`, `inventory:<project>`, `states:<project>`, `history:<env>`, …) with loading, error and read time. A failed read keeps the last data and says why. A read that fails for some places only keeps their last variables, ends with a warning, and Rotate says above its rows that a secret there is not counted.
- **Secrets** live only in memory, in the variables collection and the change set.

### 3.3 Loaders and live data

| Data | Source and cadence |
|---|---|
| Catalog | one paginated query; at start and on refresh |
| Variables and references | one aliased discovery query per project, projects in parallel; at start, on refresh, after a run |
| Environment config | `environment(id).config` per environment, with the inventory |
| Deployment states | every 10 s per project, plus websocket events |
| Metrics, volume usage | every 30 s, the last hour at 30 s samples |
| History and members | lazily per environment, paged |
| Logs | an `environmentLogs` subscription while the Logs screen is open |

- A loader is a `tea.Cmd` that returns a message. Periodic loaders reschedule with `tea.Tick` while their screen is visible and stop when it is not.
- The inventory reads **every environment of the selected projects**, because database detection and URL matching need them (a staging URL may point at the production database). Screens other than Rotate still show only the selected environments: one function decides what is read, another what is shown. An environment the token cannot read does not fail its project.
- One websocket carries all subscriptions. It opens with the first subscription, closes with the last, and reconnects after 1, 2, 5, 10, then 30 s.

### 3.4 Change set and kinds

The change set is a plain ordered list. It holds no execution order, phases or redeploys; structure comes from the kinds.

```go
type Change struct {
    ID       string          // 16 random hex characters
    Kind     string          // "variable.set", "limits.set", "db.password", ...
    Place    domain.Place
    Target   string          // variable name, "limits", volume, domain, database user, "" for actions
    New      json.RawMessage // kind-specific: the value, limits, size, password source ...
    Base     Base            // what the target was: fingerprint for secrets, value otherwise, or absent
    Origin   *Origin         // who made it, e.g. a Rotate row and its letter
    Group    string          // changes that belong to one intent
    Requires []string        // ids of changes this one needs (a URL needs its password change)
    RunID    string          // set while a run holds the change
    Flag     string          // why the last run kept it
    Created  time.Time
}
```

**A change is resolved when it is made and is self-contained after.** Creating a change is the only moment it is resolved against the store. Generated values are fixed, a copy takes the source's value and fingerprint, references `${{…}}` are parsed, a database password records how to reach its database, and links go into `Requires`. From then on, everything that reasons about change sets without Railway (writes, reads, partial apply, conflicts) is a pure function of the change data.

**Merging.** `ChangeSet.Put` merges a change into an existing one on the same place and target. The newer replaces the older but keeps the older's base, ID and creation time, so drift is measured against what the user first saw. If the older was skipped as drifted, the newer keeps its own base and the flag clears. An edit with no group or origin (made in Variables, say) keeps the older's group and origin. A change back to its base value is removed.

**Groups** tie changes of one intent together, most often every place of one leaked value. A group is soft: rotating a secret in 4 of its 7 places is allowed and shown as a warning (`JWT_SECRET · 4 of 7 places · 3 keep the old value`), never refused.

**Kinds** implement `apply.Kind`:

| Method | Purpose |
|---|---|
| `Validate` | Check against the store: the place exists, the name is valid, references resolve, `RAILWAY_*` is read-only, a database root password cannot be deleted. |
| `IsNoop` | Whether the change is back at its base. |
| `Overlay` | Short and long display, with a tone. |
| `Secret` | Whether the new value is a secret, which decides masking. |
| `Writes`, `Reads` | Targets written and read, pure functions of the change data. |
| `Check` | Read Railway and compare the target with the base and the new value: unchanged, already new, or changed. |
| `Patch` | A fragment of the environment's config patch, if the kind goes through the patch. |
| `Redeploys` | Places that need a redeploy because of this change. |
| `OwnSteps` | Steps outside the patch. |

Optional interfaces add data-only validation, drift against the store, the new value's fingerprint, a custom skip reason, step reopening and marker kinds.

| Kind | Applied through | Redeploys |
|---|---|---|
| `variable.set`, `variable.delete` | environment patch | its place and every place that references it, transitively |
| `limits.set` | environment patch (`deploy.limitOverride.containers`) | its place |
| `volume.size` | environment patch (`volumes.<id>.sizeMB`) | its place, unless the volume allows online resize |
| `domain.service.add/delete`, `domain.custom.add/delete` | own step (domain mutations) | none |
| `service.redeploy`, `service.restart` | own step | they are the action |
| `db.password` | own steps: preflight, dbpw, dbverify | none itself; its variable writes redeploy their places |
| `db.keyfile` | the KEYFILE variable writes, then a verify | every replica-set member |
| `variable.keep` | nothing: a recorded decision | none |

**Persistence.** The change set is saved to `changeset.json` (0600, in a 0700 directory, because it holds new secret values) after every change, atomically: write a temp file, fsync, rename, fsync the directory. It survives restarts. At start, changes are checked against their own data, then flagged when the fresh inventory shows their target drifted. A drifted change is never silently dropped.

**Visibility.** The footer of every screen shows the change set when it is not empty (`● 5 changes · 3 services · 2 envs`); `C` opens the Change set screen from anywhere. Rows with pending changes carry a mark.

### 3.5 Compiler: change set to DAG

**Partial apply.** The user applies the whole change set or a selection: by environment, service, group or single changes. The selection is checked against the unselected rest with the same `CheckConflicts` used between runs, so a selected change that reads what an unselected one writes is refused with the reason. Selecting only some changes of a group is allowed, with the coverage warning.

`apply.Compile` turns the frozen selection into a graph:

| Step | Key | Depends on |
|---|---|---|
| environment patch | `patch:<env>` | the write steps serving each change's `Requires` (e.g. a password's `dbpw`) |
| a kind's own write step (domain, database) | kind-specific | what the step declares |
| redeploy | `redeploy:<place>` | the write steps of every change at that place or redeploying it, and of every change sharing a group with those |
| restart | `restart:<place>` | the same; a restart at a place that also redeploys collapses into the redeploy |
| database steps | `preflight:`, `dbpw:`, `dbverify:<db place>:<user>` | see section 6 |

- All patchable changes of one environment go into one `environmentPatchCommit` with `skipDeploys: true`. The commit message marks the run: `railway-manager <run id>: N changes`. History uses it to recognise the app's own patches.
- Redeploys are derived from the kinds' `Redeploys` plus explicit redeploy changes, minus the places the user switched off on the Change set screen (`RedeployOff`, stored in the change set and frozen into the run). After its commit, the patch step re-reads the environment and adds a redeploy for every place that now references a written variable, even a reference added meanwhile in the dashboard.
- **Half-applied rule.** A redeploy depends on the writes of every change in the same group, in any environment. If a write of a group fails anywhere, every redeploy of that group is blocked, so no service runs with half of a rotated secret. Database password groups are the exception (section 6.4).
- A dependency on a step absent from the graph is dropped. A cycle is an error.

### 3.6 Executor, journal and resume

- **Check before act.** Every step first reads the real state (Railway or the database) and records `done` without acting when the effect is already there. Then it acts, then it confirms. Sealed variables cannot be read back; a kind may confirm them in its own way.
- **Mark before acting.** A step journals `Acted` right before it sends a state-changing command, so a crash after the command still shows that it may have acted.
- **Statuses:** `pending`, `started`, `done`, `failed`, `blocked`, `skipped`. A step runs when every dependency is `done` or `skipped`. A step whose dependency failed is `blocked`.
- **Concurrency.** One executor per process runs every active run with a shared limit of 6 steps. Redeploys, restarts and database steps lock their place, so one service is never redeployed twice at once.
- **Journal.** `runs/<run id>/journal.json` holds the run state and, per step, status, error, timestamp, Railway patch id, step data such as tunnel pids, and `Acted`. It never holds a value. Every status change is written atomically before the next call.
- **Resume** rebuilds the same DAG from the run's frozen change set (same step keys), skips `done` steps and runs the rest. `started` steps run again, because they check first. A patch step that committed in an earlier attempt waits on that patch first, so a retry never sends a second patch while the first still applies.
- **Drift.** A patch step checks each change against its base. A drifted change that no patch of this step has carried yet is `skipped` with the reason, and nothing that depends only on it runs. A drifted change that an earlier patch of the step did carry **stops the run** for a human to look at.
- **Run lifecycle.** A run is `running`, `failed` (resumable), `abandoned` or `finished`. Its changes are read-only in the live change set until it finishes or is abandoned. Abandoning makes them editable again and leaves whatever was already written in place; the next apply's check-before-act sees it.
- **After a run.** A change leaves the change set only when every step serving it, including the redeploys it caused, is done. Otherwise it stays with a flag: `skipped: <reason>`, `written, not deployed` or `failed: <error>`. Resuming a "written, not deployed" change retries only the redeploy, because the write checks first.
- **Several runs at once.** Before a run starts, `CheckConflicts(candidate, active)` compares it with every unfinished run, using only `Writes` and `Reads`. Two sets conflict when both write the same target or one writes what the other reads. A conflict refuses the new run and names the other run and the target. Redeploying the same service from two runs is not a conflict: the place lock serialises it.
- **Preparing a run** happens off the UI goroutine. It takes `prepare.lock` in the work dir, checks and compiles on a clone of the change set, refuses if the live set's `Rev` moved meanwhile, locks the selected changes under a new run id, freezes the run to disk, then releases the lock.
- **Run lock.** Each running run holds `runs/<id>/lock` with its pid, published atomically through a hard link. A lock whose pid is dead or was reused is taken over under a `flock` guard. Pid reuse is detected by comparing the process start time with the lock's write time, with a margin against clock steps. Two app instances therefore never execute one run, and another instance's running runs show as "elsewhere".
- **Waiting on the outside world.** A redeploy refused because a build is still running retries every 15 s for up to 10 minutes. A patch is polled every 2 s for up to 60 s. A custom domain is done once Railway created it; DNS and certificate status are shown, not waited for.
- **Confirmation.** Applying a change set asks for the word `apply`. An immediate action asks `y/n`.

### 3.7 TUI structure

- **One root model** owns the store, the change set, the loaders, the websocket, the runs and a **screen stack**. It applies data messages to the store, then forwards the message to the top screen. It holds no screen logic.
- **A screen** is its own package implementing `tui.Screen`: `Init`, `Update(ctx, msg) (Screen, tea.Cmd)`, `View(ctx, width, height)`, `Title`, `Keys`. Optional interfaces handle suspend/resume of polling and raw key capture for text fields. Screens read selectors through a shared `Ctx` and return intents (open a screen, add changes, apply, resume) as messages to the root.
- **Screens:** Home, Auth, Settings, project/environment picker, Services (status, charts, panel, limits, volume), Domains, Logs, Variables (by name and by value, with variable detail), History, Rotate secrets, Change set, Run, and a keybinding modal.
- **Components:** the frame (header, body, footer with a one-shot message, key hints that drop by rank with `? more`, and the change-set indicator), windowed lists, adaptive tables that cut and never wrap, masked input with repeat, a file picker that never shows file content, filters, confirmations, the stalled-run banner.
- **Theme:** a role palette (accent, muted, faint, new, danger, warn, ok, selected, border) with a color-profile fallback and `NO_COLOR`. Selection uses reverse video only on 8-color terminals.
- **Icons:** one registry with Nerd Font and plain forms; the setting is `auto`, `nerd` or `plain`.
- **Key handling** is a table per screen plus small pure functions, tested without a terminal.
- **Disk I/O never runs on the UI goroutine.** Saves, run preparation, the runs index and abandon run in commands.

---

## 4. Railway access

- **GraphQL over HTTPS** at `https://backboard.railway.com/graphql/v2`. A transient failure (network error, 5xx, 429) is retried once for queries. Mutations are sent at most once, because Railway may have processed a request whose answer was lost; the step's own check-before-act retry is the safe one.
- **Websocket** at `wss://backboard.railway.com/graphql/v2` with the `graphql-transport-ws` protocol, for deployment events and logs. Server error texts are never passed on; failures are shown as fixed texts.
- **Token sources,** in order: `RAILWAY_API_TOKEN`, `RAILWAY_TOKEN`, then the Railway CLI's `~/.railway/config.json` (re-read when its mtime changes).
- **CLI refresh.** The CLI's token is short-lived. A config token within 30 s of its expiry, or one Railway rejects with 401, is refreshed by running `railway whoami`, which makes the CLI renew it, and the request is retried once. Concurrent callers share one refresh. A failed refresh asks the user to run `railway login`. A 400 body is discarded, because validation errors can echo values.
- **Write guard.** Every mutation passes `WriteAllowed`, built from the `writeEnvs` list in Settings. A write to any other environment fails before a request is sent. With no write environments, nothing is writable. A database password change, which does not go through the API, asks the same guard for its database's environment and its group's before it touches the database, so a resumed run cannot change a password whose variables can no longer follow.
- **Operations used:** discovery via aliased `variables` queries; `environment(id).config` and `environmentPatchCommit` for variables, shared variables, limits and volume size; `serviceInstanceRedeploy`; `deploymentRestart` on the active deployment; `serviceDomainCreate/Delete` and `customDomainCreate/Delete` (domains added through the patch are ignored by Railway, so they are separate steps); `environmentHistory` and `environmentPatches` for History; metrics and log subscriptions.
- **Removing a limit override** uses `limitOverride: {containers: null}`; `limitOverride: null` is silently ignored.

---

## 5. Files on disk

Work dir `~/.railway-manager/` (override with `RAILWAY_MANAGER_HOME`), mode 0700. Every file is written atomically.

| Path | Mode | Holds |
|---|---|---|
| `settings.json` | 0600 | selected projects and environments, `writeEnvs`, internal names, leak time `since`, icons. No secrets. |
| `changeset.json` | 0600 | the live change set, **including new secret values**, and the redeploy opt-outs |
| `prepare.lock` | | held briefly while a run is checked and frozen |
| `runs/<id>/changeset.json` | 0600 | the run's frozen changes, **including values** while it may act; rewritten without them once finished, and without all but its database passwords once abandoned (a revert logs in with one) |
| `runs/<id>/fingerprints.json` | 0600 | the fingerprints of the values a scrubbed run held, by change id; digests only |
| `runs/<id>/journal.json` | 0600 | run state and step records; no values |
| `runs/<id>/actions.json` | 0600 | the redeploys and restarts frozen with the run |
| `runs/<id>/reopen.json` | 0600 | which done steps reopen while others are pending |
| `runs/<id>/lock` | | the pid of the process executing the run |

Finished and abandoned runs are deleted 30 days after their journal was last written; a failed or running run is kept, since it may resume. At start the app removes temporary files of interrupted atomic writes, marks the work dir with `.metadata_never_index` and, on macOS, excludes it from Time Machine. A change set moved aside as unreadable is reported as holding secret values.

SSH tunnel records (socket path and pid, never a secret) live in the journal's step data. A run directory that cannot be read is reported with how to move it aside; it is kept, because it may record a database password change.

---

## 6. Secret rotation

After a leak, every leaked secret has to be replaced in one controlled pass. Rotate secrets groups identical secret values across the selected projects and environments, shows which ones need a new value, and turns each decision into a change in the shared change set. It is one more producer of changes, not a separate plan. Reopening the screen rebuilds everything from the inventory and the change set; there is no other state.

### 6.1 Grouping by value fingerprint

Values are compared through `domain.Fingerprint`, which never contains the value:

| Variable | Fingerprint |
|---|---|
| sealed | `sealed:<service or shared>:<env>:<name>` |
| reference (`${{…}}`) | `ref:<name>:<hash>` of the unrendered text |
| literal, 20+ bytes | `<hash>`, so equal long values group across names |
| literal, shorter | `<name>:<hash>` |

`<hash>` is the first 16 hex characters of SHA-256. `RAILWAY_*` names and non-literal values are skipped. Sealed variables are kept only when the name looks secret. Shared variables group like service variables. Groups with the same names and classification merge into one row, and each distinct old value in a row gets a letter `a`, `b`, `c`… shown at every place.

### 6.2 Classes

The first matching rule wins:

| Class | Rule | Section |
|---|---|---|
| **DB** | the value is a database's root password variable | database |
| **URL** | a credential URL whose host and port name a detected database | database |
| **GEN** | a name in the internal list (built-in plus Settings) | generated |
| ASK (public) | a public-looking name (`NUXT_PUBLIC_`, `VITE_`, `…CLIENT_ID`, `…_DSN`, …); dimmed, off the list until `h` | values you paste |
| **ASK** | any other credential URL, or an external-looking name (`API_KEY`, `WEBHOOK`, `…_TOKEN`, `…_KEY`, `…_SECRET`, `LICENSE`, …) | values you paste |
| **GEN** | a name containing `SECRET` or `PASSWORD`, or ending in `_SALT` | generated |
| hidden | anything else; shown with `h` | |

The built-in internal names are `ADMIN_TOKEN`, `MASTER_PASSWORD`, `JWT_SECRET`, `APP_SECRET`, `EMAIL_LINK_SECRET`, `ENCRYPTION_SECRET_KEY`, `KEYFILE` and `QDRANT__SERVICE__API_KEY`. Some carry a warning: `JWT_SECRET` and `APP_SECRET` log every user out, `EMAIL_LINK_SECRET` breaks links in sent emails, `ENCRYPTION_SECRET_KEY` makes encrypted data unreadable. `ENCRYPTION_SECRET_KEY` and `KEYFILE` are off by default: the first decision on such a row asks `y/n` with the warning.

A credential URL is parsed with both the shortest and the longest password reading. When the two disagree about the host, the URL is "unclear": its candidate hosts are used to match databases but never shown, and it is never rewritten automatically.

**Database detection** looks for a literal root password variable: `MONGO_INITDB_ROOT_PASSWORD` (Mongo, port 27017), `POSTGRES_PASSWORD` (Postgres, 5432 or `PGPORT`), `REDIS_PASSWORD` (Redis, 6379 or `REDISPORT`). A database's hosts come from its system values: the private domain (reachable only inside its environment) and the public TCP proxy. Mongo replica-set members are the places in the same environment with `REPLICA_SET_NAME` and `KEYFILE`.

**Generation.** A GEN value is generated with `crypto/rand` at the old value's length, at least 24 characters: lowercase hex if the old value was hex, otherwise letters and digits. A new database password is 32 letters and digits.

### 6.3 Leak time

The leak time `since` is set in Settings (local time, stored as UTC RFC 3339). A place whose variable changed after it counts as rotated (`✓ rotated`). A row whose places all changed after it reads `✓ rotated`. Without a leak time, the header says so and copy is refused, because the app cannot tell which source still holds the leaked value.

### 6.4 Places and decisions

`enter` on a row opens its places, one line per place and name. **Every place is decided on its own**: the user may change only some services. A place without a decision keeps its old value, and the row's coverage says so.

| Key | Decision | Change |
|---|---|---|
| `g` | generate | `variable.set` with a generated value; places of one letter share one new value |
| `p` | paste (masked, typed twice) | `variable.set` with the value |
| `f` | file (bottom picker, never shows content) | `variable.set` with the trimmed file content |
| `c` | copy from another place | `variable.set` with the source's value, recording the source's location and fingerprint |
| `x` | delete | `variable.delete` |
| `u` | undo | removes the change |
| | none | no change: the place keeps the old value |

- Each change carries `Group: "rotate:<old fingerprint>"` (`"rotate:add:<NAME>"` for a variable added from Rotate) and `Origin{Producer: "rotate", Row, Letter}`.
- A pending change made elsewhere (in Variables) on a place of the row counts as that place's decision.
- **Refused:** a new value equal to any old value of the row; an empty file; a delete of a variable others reference or of a database root password; a copy from a sealed or empty source, from one of the chosen places, or from a source not changed since the leak time. A place in an environment outside `writeEnvs` is shown but cannot be decided.
- **Copy source drift.** At apply, a copy whose source changed since the decision is skipped (`copy source <env> · <service> changed`), so a leaked value is never copied by accident.
- **Coverage** of a `rotate:` group is the places holding its old value (by fingerprint) against the places with a change. It appears on the places screen, the detail panel, the Change set screen and the apply prompt, and a place found by a later refresh raises it too.

`space` on a GEN row generates for every decidable place, or undoes the row's decisions when all are decided. `A` opens the Change set with every rotate change selected.

### 6.5 References

The detail panel lists the places that reach a secret through `${{…}}` references, transitively. They are not decided separately; they are redeployed with the new value because the variable kind's redeploys follow the reverse reference graph, and the patch step re-reads it after committing.

---

## 7. Database passwords

When a database password leaks, the password must change inside the database and every place that carries it, in every environment, must follow. The worst case is a change that stalls halfway: the database has the new password, but the apps did not get it. The design is built to never stall silently and to always finish forward.

Supported: root passwords of Mongo, Postgres and Redis; passwords of other users seen in connection URLs; and the Mongo `KEYFILE` of a replica set.

### 7.1 Password rows

Rotate lists one row per password (`↻ DB` for a root, `⇄ USR` for a user, `↻ KEY` for a KEYFILE). Its places come from every environment of the selected projects; the row is listed when its database, or any URL that follows it, is in a selected environment. The first line is the password itself: `g` generate, `p` paste, or `o` "already changed by hand", which writes Railway without touching the database and is marked in red everywhere.

Every other place has a mode:

| Mode | What happens |
|---|---|
| rewrite | a literal URL on this database with this user: its password is replaced automatically |
| follows | a `${{…}}` reference to a rewritten URL or to the password variable; follows by itself |
| copy | a literal copy of the password (20+ characters) in the database's environment; set to the new password |
| unrecognised | carries the current password but is not known to follow it (a URL on an unknown host, a short or remote copy, an unclear URL); needs a decision before the run |
| other | the same database with another user; shown dimmed, untouched |
| other db | the same value at another database; shown, never counted |

Any rewritten or unrecognised place can take a decision of its own, as in section 6.4 (`p`, `f`, `c`, `x`, `u`), and that decision wins over the rewrite. `k` keeps a place as it is: it writes nothing (a `variable.keep` marker change), so it is the only decision allowed in a read-only environment, and it reads red `old password stops working`. A password with a place in a read-only environment cannot run until that place is made writable or kept.

### 7.2 URL rewrite

`rotation.RewriteURL` replaces only the password of `scheme://user:password@hosts…`. The scheme, user, host list and everything after the hosts stay byte-identical. The new password is percent-encoded, leaving only RFC 3986 unreserved characters as they are. An unclear URL is refused. A rewrite is computed when the decision is made; if the URL in Railway changed before apply, the patch step re-makes the rewrite on the URL Railway holds now, as long as it is still a clear credential URL of the same user and hosts.

### 7.3 Changes

- `db.password`: place = the database service, target = the user; `New` holds the source (`generate`, `paste`, `hand`, or `revert`), the value, and how to reach the database (kind, port, root user, members, private host, the places whose redeploys the verify waits for). The value is a secret like any variable value.
- The database's own password variable, every rewrite and every copy are `variable.set` changes with `Requires: [<db.password id>]`.
- All changes of one password share `Group: "rotate-db:<database place>:<user>"`, with their own coverage.
- A KEYFILE is a `db.keyfile` change on the database service plus `variable.set` on each member's copy.

### 7.4 Apply order

```
preflight → dbpw → patch (every environment) → redeploy (every app) → dbverify
```

1. **preflight** writes nothing. It checks that `ssh` and the client are installed (or names the install command), that every redeploy target has a deployment to redeploy, that no write of the group drifted, and that a tunnel opens and the admin logs in. Mongo must reach a writable primary; a user rotation must find the user. For `o` (changed by hand), the login must succeed with the **new** password.
2. **dbpw** changes the password inside the database, marking the step acted just before the command.
3. **patch** writes the database's own variable and every rewritten, copied or decided place, as one patch per environment.
4. **redeploy** every app that got a new value, at once. Redeploys of a password group are done only when the new deployment reports `SUCCESS` (or `SLEEPING` for a serverless service); `FAILED`, `CRASHED`, `REMOVED` or 10 minutes fail the step.
5. **dbverify** logs in with the new password once those redeploys are done. After a KEYFILE redeploy, it checks every member for a writable primary and repairs a single-node replica set that lost its config by re-applying it with a forced reconfig.

A user's steps run after the root's steps of the same run and log in with the new root password once the root's `dbpw` is done, with the current one otherwise. The fragile part (ssh, tunnel, client) runs first, while nothing has changed.

### 7.5 Tunnels and clients

- `railway ssh config --dry-run -p <project> -e <env> -s <service>` gives the `User@HostName` of the service's container.
- `ssh -N -L <socket>:<host>:<port>` forwards a **Unix socket** in a fresh 0700 directory to the database port inside the container. Only ssh can create the socket and only this user can connect to it, so no other local process can stand in for the tunnel or listen to it. ssh runs in its own process group with stdin closed, never forwards the agent or X11, and trusts only the host keys of the work dir's `known_hosts` (`StrictHostKeyChecking=yes`, no global file). A host it does not hold gets no connection: preflight fails with the keys `ssh-keyscan` read, and the Run screen shows their fingerprints; `t`, then typing `trust`, adds them, and `r` resumes.
- The tunnel is recorded in the journal before anything waits on it. A later attempt reaps recorded tunnels, but signals a pid only while its command line still carries our forward spec, so a reused pid is never killed. Close sends SIGTERM to the group, then SIGKILL after 3 s.
- Clients never get a secret in argv:

| Database | Login and change |
|---|---|
| Mongo | `mongosh --nodb --eval <script>`; the password reaches the script through the environment and is spliced in inside mongosh. The change runs `updateUser` on the writable primary with `w: 'majority'`, `wtimeout` 10 s, trying each member in turn. A user is the user of one database, the one its URLs authenticate against (`authSource`, else the URL's database, else `admin`); the change looks up only that user and the verify logs in there. A user whose URLs name different databases is changed by hand. |
| Postgres | `psql` with `PGPASSWORD` and the statement on stdin; inherited `PG*` variables are unset. The change is `ALTER USER … WITH PASSWORD '<SCRAM verifier>'`, computed locally, so the plain password is not in the SQL. With psql 16+, `require_auth=scram-sha-256` makes a server that trusts the connection fail the login instead of passing it. Logins that must prove a password go to the container's private address, because the Postgres template trusts `127.0.0.1`. |
| Redis | `redis-cli` with `REDISCLI_AUTH` and the command on stdin. `CONFIG SET requirepass` for the default user, `ACL SETUSER … resetpass` for another existing user. A login first tries a random password, which must be refused, so a Redis without a password never "proves" anything. |

- A root change logs in with the current password, then the new one, so a run interrupted after the database took the new password still works.
- Transient client errors (connection refused, reset, timed out) are retried 4 times, 2 s apart. Client output is scrubbed of every secret before it reaches an error.

### 7.6 Partial writes

Once `dbpw` is done, the old password already fails for new connections. Blocking every redeploy because one write failed would keep every app broken. So for `rotate-db:` groups the half-applied rule is replaced: a failed write blocks only the redeploys of its own places, and every place that got the new value redeploys at once. A drifted write of a password group is never skipped (that would leave an app on the old password while the run finishes): an automatic rewrite is re-made on the current URL, and any other drift fails the step before anything is sent.

### 7.7 Stalled runs

- `dbpw` is **reopenable**: while any patch step that writes the new password is not done, a done `dbpw` runs again. Setting the same password again is harmless, and this also covers a Redis that restarted and took the old password back from its start command.
- A run that failed or was interrupted after a database step acted is **stalled**. Home and the Change set show a persistent banner, for example `✗ mongo root: password changed, 2 of 4 apps not redeployed · enter continue`, and the row reads `✗ stalled`. Continue resumes only what is left. A run still executing in another app instance is shown, not continued.

### 7.8 Revert

If the writes failed for good and **no place holds the new password** (checked by fingerprint, no sealed place, and no patch of the run's environments may have written), the old password is still the one in Railway. Only then does a stalled root row offer `R` revert: it abandons the stalled run and starts a new one at once that logs in with the new password and sets the database back to the variable's value. Once any place holds the new password, only continue is offered, because reverting would break those places. A revert is never automatic, and only a root password is reverted.

### 7.9 Abandon

Any failed or interrupted run can be abandoned from the Run screen (`X`). Its unfinished changes become editable again; what was written stays. For a run whose database step may have acted, the confirmation shows the risk (`the database password may already be new; abandoning leaves places on the old one`) and requires typing `abandon` instead of `y`.

---

## 8. Security rules

- Secrets live in memory, in `changeset.json` and in a run's frozen `changeset.json`, all 0600 in a 0700 directory. The journal, tunnel records, settings and logs hold none.
- Errors never carry a value. Railway's 400 bodies are dropped; database client output is scrubbed; URL rewrite errors never quote the URL.
- No secret in argv or in a URL on a command line. Database clients get passwords through environment variables and stdin, with inherited client variables unset.
- Values are masked everywhere. `v` reveals one value, only when stdout is a terminal, and hides it on cursor move, a data refresh or a screen change. Pasted values use masked input typed twice; the file picker never shows content.
- Every string from outside (names, log lines, errors) passes `text.Sanitize`, which strips ANSI sequences, control characters and invisible characters (bidi and zero-width controls, soft hyphens, tags, Hangul fillers) before it is drawn, so one name cannot pass for another.
- Writes go only to `writeEnvs`; the guard sits in the API client, below every screen.
- Mutations are never retried blindly; steps check before they act and mark before they act.
- Tunnels are Unix sockets in private directories, and a recorded pid is signalled only while it is still our ssh. ssh trusts only host keys the operator confirmed.

---

## 9. Testing

| Layer | Approach |
|---|---|
| **Unit** | Every kind (validate, merge, overlay, patch, redeploys, drift, steps); the compiler (DAG shape for mixed sets: passwords before patches, patches before redeploys, references across services, half-applied blocking, partial writes); the executor (resume after each kind of failure, check-before-act, drift before and after acting, reopen, run locks); rotation (fingerprints, each classification rule, warnings, leak time, row merging, password rows across environments, URL rewrite with percent-encoding); text and layout helpers. |
| **Screens** | Bubble Tea models driven by messages and keys and rendered into golden files at **80 and 60 columns**, for empty, loading, populated and error states. Every golden test runs `tuitest.NoSecrets`, which fails if any fixture secret appears in a frame. The README screenshots are rendered from the same frames. |
| **Fake Railway** | `railway/railwaytest` is an in-process HTTP and websocket GraphQL server that serves the client's documents by operation name, keeps variables, patches and deployments in memory, and records every mutation and subscription. |
| **Fake process runner** | `proc.Fake` scripts `railway`, `ssh`, `mongosh`, `psql` and `redis-cli`, records every command with its environment and stdin (so tests scan them for leaked secrets), and drives tunnels, password changes, Mongo primary repair, and a kill after `dbpw` followed by a resume, end to end. |
| **Live** (opt-in) | Real Railway API calls against an environment you name. |

Fixtures use neutral names: projects `shop` and `blog`, environments `production`, `staging` and `dev`, services `api`, `web`, `worker`, `cron` and `mongo`.

### Live tests

Live tests run only with `LIVE_RAILWAY=1` (`make live`) and are configured entirely through the environment; no target is built in:

| Variable | Meaning |
|---|---|
| `RM_LIVE_PROJECT` | the project, by name or id |
| `RM_LIVE_ENV` | an environment of that project, by name or id |
| `RM_LIVE_SERVICE` | an app service the tests may give test variables, redeploy, restart, resize and add test domains to |
| `RM_LIVE_WORKER` | a second app service, for the database password tests only |

The environment must hold the shared variable `RM_LIVE_TEST_TARGET=1`; the tests refuse any environment without that marker. Before any mutation, a guard checks every change: it must target that project and environment and touch only `RM_TEST_` variables, the service's limits, redeploys and restarts, or `rm-test-<n>.example.com` domains. The database password test (`make live-db`, which also needs `LIVE_DB=1`) is the only exception: it may change the Redis root password of that environment and the variable that holds it, and sets them back. Cleanup removes every `RM_TEST_` variable it created and reports leftovers.
