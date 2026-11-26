# Stage host spec: start, upgrade, status and hotspot of the stage Pi

Ops spec for the host OS side of the stage Pi: the autostart unit, the pinned version and its upgrade and rollback, the
status command and the ACT LED, the first-time install, and the Wi-Fi hotspot fallback. It is not a use case: the
behaviour lives in scripts under `scripts/setup/` that run on the Pi's host, outside the game services.

Moved here from the use cases by #2785 (spec-simplification epic #2818). [UC-060](../use_cases/UC-060.md) (Operate the
Stage Pi) and [UC-061](../use_cases/UC-061.md) (Reach the Stage Pi over its own Hotspot) keep only their goal and the
flows of the three requirements that remain: offline start (FR-180), upgrade and rollback (FR-182) and hotspot fallback
(FR-184). FR-181, FR-183, FR-185 to FR-189 are Obsolete; what they required is specified here and stays implemented.
The text below is the former use-case text, moved without a behaviour change.

**Ids.** The former flow and rule ids are kept so that the code comments and the reasons in commit history that cite
them (`UC-060 BR-005`, `UC-061 BR-011`, ...) still resolve: a flow heading names its former id ("was UC-060 A4"), a
rule keeps its id as its heading. Inside §1 to §5 a bare `A<n>`, `BR-<n>`, `Pre-<n>` or "main step" is the former
UC-060 one, inside §6 the former UC-061 one. The acceptance checklist (§7) gives each property an `SH-<n>` id and the
tests that check it; tests of a moved flow or rule name the checklist item instead of a use-case tag.

Requirements: FR-180, FR-182, FR-184 (kept); FR-181, FR-183, FR-185, FR-186, FR-187, FR-188, FR-189 (Obsolete, #2785);
NFR-008, NFR-026, NFR-027, NFR-028; C-040, C-041, C-042, C-043, C-044, C-045.
Operator and Host instructions: [hardware-setup-guide.md](../hardware-setup-guide.md) ("Starting the stack on the Pi",
"Stage Hotspot").

## 1. Start at boot (FR-180)

### Preconditions

- **Pre-1:** The Pi runs Docker and the autostart is installed (`scripts/setup/install_autostart.sh`).
- **Pre-2:** The checkout is pinned (C-040): a release tag or an exact commit sha at or after `MIN_PINNABLE`, with `.env.local` carrying the keys (UC-070 Pre-2) and no `IMAGE_TAG` (BR-003).

### Start sequence (UC-060 main steps 1 to 5)

1. System (the autostart unit, ordered after `docker.service`, `network-online.target` and `bluetooth.service`) runs `joustmania-start.sh` as one unit; there is no second unit and no health timer.
2. System checks the pin (BR-003): the checkout is a release tag or exact sha, and the image tag derived from HEAD (BR-003) is exported and is the effective one (checked with `docker compose config --images`). It never runs `git pull`, never pulls an image and never contacts the registry (BR-001).
3. System starts the game phase first: `docker compose up -d --pull never --remove-orphans` naming every active service that is not in the observability phase (the game services, connect-proxy, dashboard, envoy and an active opt-in profile) and whose image is on the Pi (BR-001, BR-002). It then polls the game services (flag-service, flagd, controller-manager, rust-hid, python-hid when active, pairing-daemon, game-coordinator, menu, audio) until they are running, and healthy where they have a healthcheck, within the game-phase timeout (180 s for `up`, which is run under `timeout`, and the poll together; N of NFR-027 is measured on the Pi); the exit code of `up` is only a hint (FR-181).
4. System starts the observability phase (Jaeger, VictoriaMetrics, Grafana, the collector and the agent) in the same unit run, inline, with `up -d --pull never --wait --wait-timeout 300`. The phase has its own timeout (300 s). A failure here is retried once within the same 300 s budget (a slow-starting Jaeger left Grafana down, #2202); a failure of the retry or a timeout is logged and makes the state `degraded` (A1); the unit exit code stays 0 and the game services keep running.
5. System sets the state `ok` and the ACT LED solid on (C-042); the unit ends `active (exited)`. `ExecStartPost` runs `joustmania-status --led` as the unit's user to judge the ended start, then the root-owned LED helper applies that state (BR-005). The operator sees the state with `joustmania-status` (BR-005).

### Observability does not come up (was UC-060 A1)

**Trigger:** One or more observability services fail to start or never become healthy (step 4)
**Flow:**

1. System keeps the game services running; the observability `up` is retried once within its budget (if at least 30 s are left), then the failed service names are recorded.
2. System sets the state `degraded`: the ACT LED blinks at 1 Hz and `joustmania-status` lists the services that are down (FR-181, FR-183).
3. Use case ends. The game is playable; the collector and the game services do not wait for the backends (UC-035 A3).

### Pin mismatch (was UC-060 A4)

**Trigger:** The checkout is provably on a branch: HEAD is a symbolic ref (step 2). A version older than `MIN_PINNABLE` never reaches this step: its start script has no such check, and the upgrade command refuses to switch to it (A2 step 1).
**Flow:**

1. System starts nothing and removes nothing: `joustmania-start.sh` names the branch and the commit, prints the `joustmania-upgrade.sh <commit sha>` command that pins the checked-out commit, writes `result=failed` and the reason (`reason=not pinned (branch ...)`) to `start.env` and exits non-zero. The unit fails, so the ACT LED blinks at 5 Hz (`ExecStopPost`, BR-005) and systemd retries it within the start limit of A5 (each retry is refused the same way, in seconds). `joustmania-status` shows the version WARN, the unit FAIL and the reason on its last-start line, and its state is `failed`.
2. Operator fixes the pin (A2 with a release tag or sha, or A3) and the upgrade command restarts the unit.
3. Use case ends. When git cannot read the checkout (no `.git`, a damaged object after a power loss, no git binary, a checkout owner git distrusts), no branch is proven: the start fails open, logs a WARN, starts with the `IMAGE_TAG` of `.env`/`.env.local` and ends `degraded` (ACT LED 1 Hz) with `reason=git cannot read the checkout` in `start.env`; `joustmania-status` shows the reason. A rollback (A3) to a recorded version that ran from a branch restores that branch; when that version already has this check, its start is refused here too and the operator pins with A2.

### Game services do not start (was UC-060 A5)

**Trigger:** A game container exits or is not healthy within the start timeout, or a game service's image is not on the Pi (step 3)
**Flow:**

1. System sets the state `failed` (ACT LED 5 Hz, set by `ExecStopPost`: `ExecStartPost` never runs on a failed start), records the failed services and the missing images (with the recovery command, BR-001) and leaves observability alone; the unit ends failed and systemd retries it a bounded number of times (3 in 15 minutes). The limit holds only for attempts of up to about 4.5 minutes each (the 4th start is refused only while 3 x (attempt + 30 s restart delay) <= 900 s): three attempts that each run into the 900 s start timeout would take longer than 15 minutes and never be stopped by it. Boot pulls nothing, a missing game image fails at once and a failing game phase ends at the 180 s game-phase timeout (step 3: `up` and readiness together) without starting observability, so a failing attempt (docker wait of up to 60 s plus the game phase, about 4 minutes) stays inside that bound; a full start (game phase plus the 300 s observability phase) stays inside the 900 s start timeout. The limit is for these automatic retries; the upgrade command's restarts do not spend it (BR-006).
2. Use case ends. A reboot with unchanged images still recreates 0 containers (NFR-026).

### Outcome (was UC-060 postconditions)

Success:

- The pinned version of the full stack runs; the state is `ok` or `degraded`, shown by the ACT LED and `joustmania-status`.
- A reboot with unchanged images and configuration recreated 0 containers (NFR-026).
- The LED is applied once at the end of the start (`ExecStartPost`: `joustmania-status --led`, then the LED helper). Accepted gap: a crash after the start is not shown on the LED until the next start of the unit (boot, restart, upgrade or rollback); `joustmania-status` shows the live state at any time, and the operator can refresh the LED by hand with `joustmania-status --led; sudo /usr/local/lib/joustmania/joustmania-led from-request` (no health timer, by decision).
- A clean stop of the unit (`systemctl stop`, shutdown) puts the board's own ACT trigger back (BR-005).

Failure:

- The state is `failed`; no container was removed that was running before the boot, and the checkout still names a pinned version.

### Rules

#### UC-060 BR-001: Offline is not a failure

With no network, or with no new image, the boot recreates nothing. Boot never pulls: no `git pull`, no `docker compose pull` and no registry contact (the Pi has no RTC, so the clock can be hours stale until NTP syncs and TLS to a registry fails; an online boot gains nothing either, because the pin decides the version, C-040). Every `up` runs with `--pull never` on the images that are on the Pi. An image that is missing is named (service and image) together with the recovery command `scripts/setup/joustmania-upgrade.sh <pin>` (the release tag at HEAD, else the commit sha), which pulls it when the Pi is online (A2 step 5); boot never waits on the network for it. The Pi must start the game from local images alone (FR-180).

#### UC-060 BR-002: Missing images only start what exists

Services whose image exists still start, and nothing is torn down. `up` starts a named service's `depends_on` services and aborts as a whole on a missing image, so a service whose dependency chain reaches a missing image is not started either; it is named ("needs a service whose image is missing") and everything else starts. A missing game image, or a game service left out this way, makes the state `failed` (A5); a missing observability or agent image, or an observability service left out this way, makes it `degraded` (A1). Docker not reachable (daemon down, the user not in the docker group) is reported as such and is never taken for missing images.

#### UC-060 BR-004: Game before observability

The game services are started and judged before the observability phase, and nothing in the observability phase can fail the unit (FR-181).

**Opt-in compose profiles** (`python-hid`, `operator-pad`): the profile is switched on with `COMPOSE_PROFILES` in the git-ignored `.env.local` or a `systemctl edit` drop-in (`Environment=COMPOSE_PROFILES=...`). Set the whole comma-separated list in one place (e.g. `python-hid,operator-pad`): a unit drop-in `Environment=` wins over `.env.local`, and a profile missing from the winning list is off, never in the tracked `.env`. `up -d --remove-orphans` keeps a container whose profile was switched off, so `joustmania-start.sh` removes that container by name while its profile is off: `joustmania-python-hid` (#1402) and `joustmania-operator-pad` (#2050: an old pad container would keep its exclusive grab of the pad and keep sending commands, [UC-015](../use_cases/UC-015.md) Pre-1). Tests: `scripts/tests/test-pi-start-compose-files.sh`, `scripts/tests/test-pi-start-safe.sh`. The pad is not a game service: it has no healthcheck and an upgrade does not wait for it.

## 2. Upgrade and rollback (FR-182)

### Upgrade (was UC-060 A2)

**Trigger:** Operator runs the upgrade command with a target release tag or exact commit sha, which changes the pin (step 2)
**Flow:**

1. System refuses the target when it is `latest`, a branch name, or older than `MIN_PINNABLE` (C-040), and changes nothing.
2. System pulls the target's images first (while the old version keeps running), then switches the checkout to the target; `IMAGE_TAG` derives from the checkout, so no environment file is rewritten.
3. System applies the new version by restarting the unit (BR-006: the start limit is reset first, and a start systemd refuses is reported as refused, never judged unhealthy) and checks the result as in main steps 3 to 5.
4. System removes superseded `joustmania` images only after the new version is healthy; the previous version stays recorded for A3.
5. Use case ends. A failed pull (no network, missing image) changes nothing: the checkout and the running version stay as they were. A target that is the version already checked out pulls only when images of it are missing on the Pi, then restarts the unit and judges it as in step 3 (no rollback: the version does not change); with nothing missing there is nothing to do. This is the recovery for a missing image (BR-001). Re-running it for the version already checked out also repairs a record an interrupted upgrade left: previous = the recorded current, or the target of a rollback that was cut off before it; a stale rollback marker is dropped. The unit is not restarted by the repair.

### Rollback (was UC-060 A3)

**Trigger:** The new version is not healthy after A2 step 3 (automatic), or the operator runs `joustmania-upgrade` with the rollback option (step 2)
**Flow:**

1. System switches the checkout back to the previous recorded version, whatever its version (the BR-003 floor does not apply to a rollback) (on its branch when it ran from one, so the image tag is the one it ran with) and restarts the unit (the start limit reset first, BR-006), with the old images (still on the Pi, A2 step 4); when an image that version resolves is missing, it switches the checkout back and starts nothing. It refuses to move a branch that has moved on since, and a refused or failed switch reports that nothing changed. The record names the rollback target from before the checkout until the restart is judged, so a rollback cut off in between (power loss, ssh hang-up) is finished by the next rollback on the same version and never toggles back to the failed one.
2. System sets the state `failed` if the old version does not come up either; otherwise `ok` (or `degraded`, A1) and reports that the upgrade was rolled back. A start systemd refuses (BR-006) is reported as refused with the recovery command, not as a failed version.
3. Use case ends. With `--no-rollback` the failed new version is left in place for inspection.

### Host files are named, never written (#2858)

The upgrade and the rollback switch only the checkout and the images; they never write host files (`/etc`,
`/usr/local`), since those come from the host setup (§4) and the hotspot setup (§6). After the switch they name what the
operator re-runs: the hotspot setup when its installed copies differ (UC-061 BR-006), and, after a healthy upgrade and
after a `--rollback`, the operator pad's udev rule `/etc/udev/rules.d/99-joustmania-operator-pad.rules`
([UC-016](../use_cases/UC-016.md), #2748) when it is missing or differs from the checkout's
`scripts/setup/99-joustmania-operator-pad.rules`. That hint names the fix: `sudo scripts/setup/setup_runtime.sh`, or
`sudo install -m 0644` of the rule, `sudo udevadm control --reload-rules` and
`sudo udevadm trigger --subsystem-match=hidraw`. A version without the rule (before #2748) prints nothing; the rule is
harmless to it. The upgrade command runs the outgoing version's code, so the first upgrade away from a version without
this hint does not print it; `joustmania-status` (§3) shows the rule on every run.

### Rules

#### UC-060 BR-003: Pin to a release or an exact commit

The pinned version is a release tag (`joustmania-vX.Y.Z`, image tag `X.Y.Z`) or an exact commit sha (image tag the 40-character sha). `latest` and `main` are refused. `MIN_PINNABLE` is **0.8.0**, the first release that contains #2040, because older releases have no `agent` image; the value is recorded here and in `MIN_PINNABLE` of `scripts/setup/joustmania-upgrade.sh`. The floor is inclusive: `joustmania-v0.8.0` is accepted, `joustmania-v0.7.0` is refused. Besides the version, the upgrade command refuses a target that does not contain the tag `joustmania-v0.8.0` once that tag is in the clone after `git fetch --tags` (before the cut only the version floor and the #2040 commit floor, `02e7752d`, apply), and always a target without the #2040 commit; it always refuses a target without `scripts/setup/joustmania-upgrade.sh`, because the Pi could not be upgraded or rolled back from it. In pin mode the image tag derived from HEAD is exported and wins over `.env` and `.env.local`; an `IMAGE_TAG` in `.env.local` that differs is ignored and reported as a status WARN (not A4). The floor applies to an explicit upgrade (A2) only: `--rollback` (A3) restores the recorded previous version whatever its version, so a Pi pinned at a pre-0.8.0 commit that contains #2040 returns to it with `--rollback`, while an explicit upgrade to that sha is refused once the tag exists. Boot never changes the pin (C-040).

#### UC-060 BR-006: Operator restarts do not spend the start limit

The unit's start limit (3 starts in 900 s, A5) stops a crash loop of automatic retries; it is never weakened. The upgrade command's restarts (A2 upgrade, A3 automatic rollback and `--rollback`) are deliberate, so each runs `sudo systemctl reset-failed joustmania` first, and an upgrade, a rollback and another upgrade inside 15 minutes all start. When systemd still refuses a start (`systemctl show -p Result` is `start-limit-hit`), the command resets and retries once; a second refusal is reported as "refused by systemd" with the recovery command `sudo systemctl reset-failed joustmania && sudo systemctl start joustmania`, and is never taken as an unhealthy version: no automatic rollback follows a refused start. `joustmania-upgrade --status` shows `start-limit-hit` with the same recovery command (#2102).

## 3. Status command and ACT LED (was FR-183)

`joustmania-status` and the ACT LED tell the operator whether the stack is starting, ok, degraded or failed, without a keyboard and screen (C-042).

#### UC-060 BR-005: One status vocabulary

The state is `starting`, `ok`, `degraded` or `failed`. Under `/sys/class/leds/ACT` (fallback `led0`) the script writes `trigger` first, then `delay_on` and `delay_off`: `starting` = trigger `heartbeat`; `degraded` = `timer` 500/500 ms (1 Hz); `failed` = `timer` 100/100 ms (5 Hz); `ok` = `default-on` (solid on, not a restored saved trigger). With no LED (dev box) or no permission it logs one line and exits 0, and it never fails the unit. The unit sets `starting` in `ExecStartPre`; in `ExecStartPost` `joustmania-status --led` (as the unit's user) judges the ended start (a unit still `activating` counts as started, so the state is never `starting`; a service that is running but whose health check is still `starting` is WARN, so a game service in that state gives `degraded`, not `failed`, #2135) and writes it to `led.request` in the unit's runtime dir, and the helper applies it (a request that is not a valid state: `degraded`; no request, as after a rollback to a version without `--led`: the `result` of `start.env` when `ok` or `degraded`, else the board's trigger); the status call is bounded (`timeout 120`), because a timed-out `ExecStartPost` would stop a ready game. `ExecStopPost` restores the board's own trigger when `$SERVICE_RESULT` is `success` (a clean stop) and sets `failed` for any other result (a failed or timed-out start, and also a stop whose `compose stop` failed). An installed unit without these hooks shows no LED cue; the start itself is unchanged. The board's trigger is saved at the first LED write after boot and only that is restored; with nothing saved a stop changes nothing. Writing the LED needs root: `install_autostart.sh` installs a root-owned copy of `scripts/setup/joustmania-led.sh` as `/usr/local/lib/joustmania/joustmania-led` and the unit runs only that copy with the `+` prefix (no sudoers rule; the checkout, writable by the unit's user, never runs as root). The unit's runtime dir belongs to the unit's user, so the helper, as root, opens `led.request` and `start.env` there without following a symlink and without blocking (`O_NOFOLLOW`, `O_NONBLOCK`), reads at most a bounded size within 2 s, and only compares the content with the state words: a path swapped for a symlink (for example to `/dev/watchdog`) or a FIFO, also between its check and the open, reads as no valid request (#2135). `joustmania-status` (`scripts/setup/joustmania-status.sh`, read-only; with `--led` it writes only `led.request`) prints one `OK`/`WARN`/`FAIL` line each for the pinned version and its image tag (WARN on a branch, A4), the unit (FAIL on `start-limit-hit` with the BR-006 recovery command), the last start recorded in `start.env`, missing images (with the BR-001 recovery command), every active service (running, health; a running but `unhealthy` one with its recovery `docker restart <container>`, since compose restart policies do not act on health, #2125) and the clock (INFO only when not NTP-synchronised: the Pi has no RTC and that is accepted, so it never changes the state), then `state: ok|starting|degraded|failed`; exit 0 ok, 1 starting or degraded, 2 failed. `degraded` means A1 (an observability or agent service down or its image missing) or A4 (a branch checkout), and with `--led` also a game service that is running but whose health check is still `starting` (#2135); `failed` means a game service down or its image missing, the unit failed or start-limit-hit, or docker not reachable (then images and services are not checked). While the unit is still activating, a game service that is not running yet is INFO and the state is `starting` (C-042, FR-183). The same helper also carries the UC-061 hotspot cue (`hotspot-failed`, `hotspot-clear`; UC-061 BR-011): it records the cue apart from the state, takes a lock in its state dir for every change (it waits at most 10 s for it, then goes on without it, so a stuck holder never blocks the unit's hooks) and shows the winner, highest first `failed`, `starting`, the hotspot cue, `degraded`, `ok`, the board's trigger; the cue never changes `state:` or the exit code. An INFO `led:` line shows the state the helper recorded (with `+ hotspot cue` while the cue is set) and the live trigger, and an INFO line names an installed helper that differs from the checkout (re-run `install_autostart.sh`; the upgrade command says so when the unit or the helper changed).

An INFO `operator pad rule:` line (#2858) compares `/etc/udev/rules.d/99-joustmania-operator-pad.rules` with the
checkout's `scripts/setup/99-joustmania-operator-pad.rules` and says `installed`, missing (the pad screens of
[UC-016](../use_cases/UC-016.md) cannot work), differs from this checkout, or not readable; missing and differing name
the fix of §2 ("Host files are named, never written"). It never changes `state:` or the exit code: the game runs
without the pad screens. `JOUSTMANIA_PAD_RULE` overrides the installed path for tests only.

## 4. First-time install and development setup (was FR-188)

### First-time install (was UC-060 A6)

**Trigger:** Operator runs `./setup.sh` in a fresh clone on a new Pi, before the autostart can run (step 1); Pre-1 does not hold yet
**Flow:**

1. Operator picks the runtime setup (`scripts/setup/setup_runtime.sh`: Docker and the compose plugin, Bluetooth configuration (`dtoverlay=disable-bt` appended once, `ClassicBondedOnly=false` set, created or appended when missing; a reboot is asked for when the overlay was added) and the audio volume, and Docker's default log limit, `scripts/setup/docker-log-defaults.sh`: json-file 10m x 3, merged into an existing `/etc/docker/daemon.json` with its keys kept, a file with another log driver or invalid JSON left alone, docker restarted only when the file changed) or the full development setup (`scripts/setup/setup_host.sh`). When the setup script fails, `setup.sh` exits non-zero and offers nothing after it: no env file, no autostart, no pin and no reboot.
2. System offers to create `.env.local` from `.env.example` with mode 600 (the decision-backend keys and the opt-in profiles, never `IMAGE_TAG`, BR-003); an existing `.env.local` is left as it is.
3. System offers the autostart (`sudo scripts/setup/install_autostart.sh`, Pre-1).
4. With the autostart installed, System offers the pin: the version a pinned checkout already has, else the newest release (`joustmania-vX.Y.Z`) that contains the pinnable floor of the upgrade command, else the commit sha of the clone. It runs A2 for it while the Pi is online: the images are pulled and the unit is started, so the first boot needs no network (BR-001). Without the autostart, a pinned checkout gets its images pulled (`joustmania-compose.sh pull`).
5. System ends with the commands still to run, the same whichever step left them (the pin when docker is not usable by the user yet, because the docker group needs a new login, or when the pin or the pull was declined; without the autostart on a branch, the autostart and the pin) and with the full-stack commands (`systemctl start joustmania`, `joustmania-compose.sh up -d`, `joustmania-status`); the lite stack is named for manual tests only (C-041). When the autostart is installed on a branch checkout and the pin is left for later, System says that the next boot starts nothing until the pin has run (A4). A failed step makes the exit code non-zero, and the development setup then does not reboot.
6. Use case ends. `setup.sh` refuses to run as root, and in a checkout the user does not own it names `scripts/hardware/update_permissions.sh` (the upgrade command needs the checkout owner).

### Development setup of the host and repair of the checkout owner (was UC-060 A7)

**Trigger:** Operator runs `scripts/setup/setup_host.sh` (the full development setup, picked in `./setup.sh`, A6 step 1) or `scripts/hardware/update_permissions.sh` (the owner repair, A6 step 6), before the autostart can run (step 1)
**Flow:**

1. System (`scripts/setup/setup_host.sh`, fail-fast: `set -e`, and `|| exit 1` on the installs) runs nine numbered steps, each with `sudo` where it needs root: (1) removes `realvnc-vnc-server`; (2) `apt-get update` and `upgrade`; (3) installs the build, audio and Bluetooth packages (Python, SDL, PortAudio, BlueZ, ffmpeg, `jq`, ...); (4) installs Docker with `get.docker.com` when `docker` is missing and adds the user to the docker group (a new login is needed); (5) installs the Python tools and creates `venv` (system site packages) in `~/JoustMania`; (6) installs `uv` when missing and runs `uv sync`; (7) sets every ALSA mixer control to 100 % unmuted and stores the ALSA state; (8) appends `dtoverlay=disable-bt` to the boot `config.txt` when absent (`/boot/firmware/config.txt` on Debian 12 and newer, else `/boot/config.txt`), disables `hciuart` on Debian 12 and older, and rewrites an existing (or commented) `ClassicBondedOnly=true|false` line in `/etc/bluetooth/input.conf` to `ClassicBondedOnly=false` (it adds no key when none exists, and a missing file fails the step); saved Bluetooth registrations are kept (#1443); (9) states that the pairing daemon runs as a container (no host install).
2. System then makes the checkout owned by the user when `setup.sh` is owned by root, installs the supervisor configuration from `conf/supervisor/` when that directory exists, and ends with the command to start the stack. `./setup.sh` then reboots after 5 seconds (A6 step 5: not after a failed step).
3. Use case ends. `scripts/hardware/update_permissions.sh` is the repair for a checkout owned by another user (A6 step 6): `sudo chown -R "$USER:$USER" .` on the current directory, so it is run from the checkout. The development setup is not part of the stage Pi's boot path (Pre-1).

## 5. Code and tests of §1 to §4

**Code / tests:** `docker-compose.yml`, `docker-compose.lite.yml` (BR-004: no game service and no collector `depends_on` an observability service; every service has `restart` and a log limit, #2027); tests `tests/contract/test_compose_startup.py` (BR-004, FR-181). `scripts/setup/joustmania.service`, `scripts/setup/joustmania-start.sh` (main step 1, A5, Post-F-1: no `rm`/`prune`/`down`, targeted cleanup of joustmania-* containers that are not our own (own = listed by `compose ps -a`, or from this checkout's compose working dir under a project `compose ps -a` lists; one from this checkout under another project name is removed), BR-001: no `git pull`, no `compose pull`, every `up` with `--pull never`, a missing image named with the `joustmania-upgrade.sh` recovery; BR-002/BR-004, main steps 3 and 4: phase lists `JOUSTMANIA_GAME_SERVICES` / `JOUSTMANIA_OBS_SERVICES` in `lib-joustmania.sh`, service images from `compose config` parsed in memory, game `up -d --pull never --remove-orphans <services>`, readiness poll of `compose ps`, then the observability `up --wait`; `start.env` adds `missing=` and `down=`; `docker info` must answer first; a service whose `depends_on` chain reaches a missing image is left out of its phase and named); tests `scripts/tests/test-pi-start-safe.sh` (main step 2, A5, Pre-1, Post-F-1, NFR-026: a second start with unchanged images runs the same `up` without a forced recreate or a removal), `scripts/tests/test-pi-start-offline.sh` (main steps 1 to 5, BR-001, BR-002, BR-004, A1, A4, A5, Pre-2, Post-S-1, Post-F-1), `scripts/setup/joustmania-status.sh` with `scripts/tests/test-joustmania-status.sh` (BR-005 incl. `--led` and the `led:` line, A1, A4, A5, BR-006), `scripts/setup/joustmania-led.sh` with `scripts/tests/test-joustmania-led.sh` (BR-005: every state's sysfs writes on a fake sysfs, `led0` fallback, no LED, restore, `stop-post` per `$SERVICE_RESULT`, `from-request`, a request or `start.env` swapped for a symlink between the check and the open, #2135; the hotspot cue and its precedence, and a change waits while another holder has the lock, UC-061 BR-011), `tests/contract/test_joustmania_unit.py` (A5, Pre-1, Post-F-1, BR-005: the LED hooks, their `-`/`+` prefixes and the installed helper path). `scripts/setup/joustmania-upgrade.sh` (A2: refuses latest, branch names, short shas and targets below the floor; preconditions not root, clean tracked tree, `git worktree prune`, registry `/v2/` answers HTTP 200 or 401, `git fetch --tags`; prepare pull in a temporary worktree removed by a trap, with the active `COMPOSE_PROFILES` (shell, `.env.local` or the unit's `Environment=`), aborting with the missing images listed; `git checkout --detach` is the single switch; after it `.joustmania-release` records current and previous (ref, branch, image tag, images); `sudo systemctl restart joustmania`, then every active game service must be running and healthy within the timeout (no service list, or none with a game service, is not healthy); prune removes only images of the `joustmania` repositories the two recorded versions use whose tag neither uses, and nothing without a complete record; WARNs for `IMAGE_TAG` in `.env.local` and a set `OTEL_COLLECTOR_TAG`. BR-006: `sudo systemctl reset-failed joustmania` before every restart, a `start-limit-hit` refusal reset and retried once, then reported as refused (no rollback), `--status` shows `start-limit-hit` with the recovery command. A3: automatic rollback when not healthy, `--no-rollback`, `--rollback` with local images and no network, targeting the recorded version that is not HEAD; a version that ran from a branch is restored on that branch; the recorded images are checked before and the images compose resolves after the switch, else it switches back and nothing restarts), `scripts/setup/joustmania-compose.sh` + `scripts/setup/lib-joustmania.sh` (BR-003: a detached HEAD exports `IMAGE_TAG` = X.Y.Z of its `joustmania-vX.Y.Z` tag, else the full sha, over `.env`, `.env.local` and the shell, with a stderr note when a shell `IMAGE_TAG` differs; nothing on a branch), `scripts/setup/joustmania-start.sh` (A4: a branch checkout starts nothing, `start.env` carries the reason; git that cannot read the checkout fails open, `degraded`; tests `scripts/tests/test-pi-start-offline.sh`, `scripts/tests/test-joustmania-status.sh`; an `IMAGE_TAG` in `.env.local` is logged as a WARN, BR-003); tests `scripts/tests/test-joustmania-upgrade.sh` (A2, A3, A4, BR-003, BR-006). `setup.sh`, `scripts/setup/setup_runtime.sh`, `scripts/setup/docker-log-defaults.sh` (A6); tests `scripts/tests/test-setup.sh` (A6: a failed setup stops, a run as root and a checkout not owned by the user (names `scripts/hardware/update_permissions.sh`) exit 1, a pinned checkout keeps its own version, `.env.local` mode 600, the pin target and its order after the unit, docker not usable yet, the declined pin) and `scripts/tests/test-setup-scripts.sh` (`JOUSTMANIA_BOOT_CONFIG` and `JOUSTMANIA_BT_INPUT_CONF` are test seams: unset in production, not a tunable, C-035) (A6: the runtime setup's step order, the Bluetooth config on a fresh Pi and on a rerun, a failed apt install stops it; A7: the host setup's nine steps in order, root only through sudo, `update_permissions.sh` chowns the current directory) and `scripts/tests/test-docker-log-defaults.sh` (A6: write, merge with existing keys kept, unchanged rerun, another driver, invalid JSON).

**Hotspot fallback:** reaching the Pi over its own Wi-Fi hotspot when the uplink has no connection is [UC-061](../use_cases/UC-061.md) (#2099). Its units are independent of the start of §1 (UC-061 BR-006), and the hotspot is not part of the state of §3 (UC-061 BR-007); its one LED effect is the hotspot cue, which loses to `starting` and `failed` (UC-061 A10, BR-011, #2137).

## 6. Hotspot fallback (FR-184)

Known gap (main steps 4 and 5, checklist SH-16): reaching the stack at `10.42.0.1` from a joined device and the convention play mode are checked only on the Pi (#2088); no automated check exists, because they need a real access point and a joined client. The ACT LED cue of A10 (#2137) is checked on a fake sysfs only; the on-Pi check is part of #2088.

### Preconditions

- **Pre-1:** NetworkManager manages `wlan0` and the uplink, and `dnsmasq-base` is installed (NM's shared mode runs it; it cannot be installed offline at the venue).
- **Pre-2:** The Wi-Fi radio can run a 5 GHz access point: the Wi-Fi country is set and survives a reboot, the configured channel is neither disabled nor "no IR" for that country, the radio is not switched off (`dtoverlay=disable-wifi`, rfkill, NM's radio switch), and the chip has 5 GHz (Pi 3B+, 4 and 5; not a Pi 3B or Zero 2 W).
- **Pre-3:** The Operator ran the hotspot setup with a password (FR-186). It installed the profile, the root-owned helper, the unit, the timer and the dispatcher hook, and its forced self-test passed.
- **Pre-4:** UC-060 is installed; its offline start (UC-060 BR-001, FR-180) is what makes convention play possible. The hotspot itself does not depend on it (BR-006).

### Fallback sequence (UC-061 main steps 1 to 6)

1. The uplink loses its connection, or the Pi boots without one. System (the dispatcher hook on an uplink up, down or `dhcp4-change` event or a `wlan0` down event, or the timer: 30 s after boot, then every 60 s) starts the check; a start while a check runs joins that check. No boot target waits for the check.
2. System classifies the uplink (BR-008) and waits until it has not been stably connected for the up delay: 20 s, or 60 s while the uplink is still connecting or was connected only briefly (NM's DHCP timeout is 45 s). In the first 45 s after boot it also waits, so that a known Wi-Fi network can connect first (A2).
3. System finds no Wi-Fi client connection up or connecting on `wlan0` and the uplink not connected at that moment (a connected uplink that is not yet stable means wait, BR-002), and brings up the `joustmania-hotspot` profile (addressed by its fixed UUID) on `wlan0`: WPA2-PSK with CCMP only and PMF set explicitly, 5 GHz on the configured channel (36 by default), address `10.42.0.1/24`, DHCP and DNS from NM's shared dnsmasq, no default route and no upstream (BR-002, BR-003, C-043, C-044). It logs one line with the reason and writes the non-secret status file `/run/joustmania-hotspot/status` (format 1).
4. Host joins the SSID with the password (on a phone with mobile data off, BR-009) and opens `http://10.42.0.1/`: the dashboard, `/jaeger/` and `/grafana/` work through envoy unchanged, and SSH on port 22 works too. `<hostname>.local` through avahi is best effort; the address is the one given in the Host instructions.
5. The game runs in convention play mode: offline, the agent on the rules backend (remote backends fail over to rules as today), observability without internet (FR-185).
6. `joustmania-status` shows the hotspot line (FR-187, BR-007).

### The cable returns (was UC-061 A1)

**Trigger:** The uplink is connected again while the hotspot is up (step 4)
**Flow:**

1. System waits until the uplink has been connected for 15 s without a break (BR-008); a short connection inside a flap does not count (A6).
2. System takes the hotspot down within the NFR-028 bound; its clients drop.
3. Use case ends. The Host switches to the LAN.

### A known Wi-Fi network is up or connecting (was UC-061 A2)

**Trigger:** `wlan0` has a Wi-Fi client connection that is up (NM state 100) or connecting (states 40 to 90) (step 3)
**Flow:**

1. System brings no hotspot up and never interrupts the client: while the client connects the check waits; while it is up the check ends.
2. A client connection counts as connectivity: each check that sees the client up marks it, and the first check that finds `wlan0` free or connecting after the mark starts the wait of step 2 again from that moment (the drop itself may have happened up to a timer interval earlier), so NM gets the full delay to reconnect the client first. When NM gives up on the client, `wlan0` is free and the check (event or timer) continues at step 2.
3. Use case ends. To use the hotspot at a venue where a known network is in range, the Operator removes or disables that client profile (Host instructions).

### The hotspot cannot start (was UC-061 A3)

**Trigger:** Bringing up the profile fails: no Wi-Fi country, the channel not allowed for an access point, 5 GHz refused by the firmware, `dnsmasq-base` missing, `wlan0` missing or its radio off, or another NM error (step 3)
**Flow:**

1. System does not fall back to 2.4 GHz (BR-003). It logs the reason without any secret, records `state=failed` and the reason in the status file, and `joustmania-status` shows it. The ACT LED shows the hotspot cue (A10).
2. System tries again on the next event and on every timer tick (every 60 s), so a `wlan0` that appears late after boot is picked up.
3. Use case ends. The game keeps running headless (BR-006). Recovery is a cable or a keyboard and screen; the ACT LED tells the Host which of the two is needed (A10).

### Carrier but no address (was UC-061 A4)

**Trigger:** The uplink has carrier but no non-link-local IPv4 address (DHCP fails, or the venue offers IPv6 only) (step 2)
**Flow:**

1. System counts the uplink as not connected (BR-008, maintainer decision D1) and, after the 60 s wait for a connecting uplink, continues at step 3.
2. Use case ends.

### Not set up or switched off (was UC-061 A5)

**Trigger:** There is no `/etc/joustmania/hotspot.conf`, it says `enabled=no`, or the profile is not installed (step 1)
**Flow:**

1. System takes a running hotspot down, records `state=disabled` or `state=not-set-up` and does nothing else. For a missing profile this happens only when NetworkManager reports that it does not exist; a failed query (D-Bus error, timeout, NetworkManager not running) changes nothing and is recorded as `state=waiting` with the reason.
2. Use case ends. The Pi behaves as without UC-061.

### The uplink flaps (was UC-061 A6)

**Trigger:** The uplink connects and disconnects repeatedly, with any number of events (step 2)
**Flow:**

1. System decides from the recorded state (since when the uplink has not been stably connected), not from the time since the last event, so new events never restart the wait.
2. System brings the hotspot up within the 60 s wait for a connecting uplink, and takes it down only after 15 s of uninterrupted connection (A1). The unit has no start limit, so a burst of events never blocks later checks.
3. Use case ends.

### The hotspot drops while in use (was UC-061 A7)

**Trigger:** The hotspot goes down while the uplink is still not connected (a firmware hiccup, or an `nmcli connection down` by hand) (step 4)
**Flow:**

1. System re-checks on the `wlan0` down event, or at the latest on the next timer tick, and continues at step 3.
2. Use case ends.

### Setup or password change over the hotspot (was UC-061 A8)

**Trigger:** The Operator runs the setup again (a new password or channel) from a device joined to the hotspot while the uplink is not connected (step 4). With the cable in (the overlap of A1) this is not A8: the setup takes the hotspot down and runs the normal path with the forced self-test; when the Operator's own session comes over the hotspot during that overlap, the setup refuses before any change (re-run over the cable LAN, or unplug the cable to re-run over the hotspot)
**Flow:**

1. System (the setup command, FR-186) keeps the fallback armed and the previous profile until the new one is checked (a dropped session or a refusal restores it from a complete copy; when the copy fails it changes nothing), refuses a channel change over the hotspot (it cannot be tested without the uplink; the Operator plugs the cable in), skips the forced self-test (there is no connected uplink to run it on; the reactivation applies and proves the new settings), finishes all file work first and reactivates the hotspot last, detached from the Operator's session (`systemd-run --on-active=5`), so a dropped session cannot leave a half installed state. A crash leaves only a hidden temp file that NM ignores.
2. System tells the Operator that the session will drop; the Operator rejoins with the new password.
3. Use case ends.

### The cable is on a network the Host cannot join (was UC-061 A9)

**Trigger:** The Pi is cabled into a network the Host's device cannot join (for example a stage or lighting switch with its own DHCP), so the uplink counts as connected and the hotspot stays down (step 2)
**Flow:**

1. Host unplugs the cable (Host instructions; maintainer decision 2026-10-27).
2. System continues at step 2 and brings the hotspot up within the NFR-028 bound.
3. Use case ends.

### The ACT LED shows a failed hotspot (was UC-061 A10)

**Trigger:** A check records `state=failed` (A3, step 3), or a later check records any other state after a `failed` one (step 3, A1, A2, A5, A7)
**Flow:**

1. System (the hotspot helper, when the recorded state changes into or out of `failed`, and again on the next check until the LED helper has answered) asks the root-owned LED helper of UC-060 (`/usr/local/lib/joustmania/joustmania-led hotspot-failed`, or `hotspot-clear`) to set or clear the hotspot cue; it never writes the LED itself (BR-011).
2. System (the LED helper) records the cue apart from the UC-060 state and shows the pattern that wins (BR-011): the hotspot cue, a short flash every 2 s, while the UC-060 state is `ok`, `degraded`, the board's own trigger or not set since boot; the UC-060 pattern while it is `starting` or `failed`. A later UC-060 state change is shown by the same rule, so the cue comes back after `starting` and survives a clean stop of the game unit.
3. When the hotspot comes up (step 3, A7) or is no longer wanted (A1 the uplink connected, A2 a Wi-Fi client, A5 disabled, not set up or removed), the cue is cleared and the LED shows the UC-060 state again, or the board's own trigger when no state is set. A reboot clears it too (both records live in `/run`).
4. Use case ends. Without the UC-060 LED helper (Pre-4 not met), with an older one that has no hotspot commands, or without an LED, the helper logs one line and no cue is shown; the next check asks again, so a helper installed later shows the cue; the hotspot works as before (BR-006).

### A Wi-Fi client is up on wlan0 during the setup self-test (was UC-061 A11)

**Trigger:** The Operator runs the setup (FR-186) while a Wi-Fi client connection is up or connecting on `wlan0`; the forced self-test (BR-001 exception) would drop it, so the helper's `selftest` refuses (step 3)
**Flow:**

1. Operator re-runs the setup with `--pause-wifi-client`, or answers `y` to the prompt the setup shows on a terminal (not with `--psk-file` or `--psk-stdin`; there the option is the only way). Without it the self-test refuses and the setup names the option; the fallback stays disarmed (A8 is separate: over the hotspot there is no client).
2. System (the setup command) refuses before any change, failing closed, when an SSH session runs over `wlan0` or when it cannot tell: it reads `SSH_CONNECTION` from its own and every ancestor's environment (sudo strips it only from its child) and every established sshd connection (`ss`), and refuses when a local address is on `wlan0`, a peer routes via `wlan0` (`ip route get`, IPv4 and IPv6) or cannot be routed, or when it has an SSH, tmux or screen ancestor but finds no peer. A mosh ancestor always refuses (its roaming UDP session may run over `wlan0`). A local console (no SSH anywhere) proceeds. Remote shells that are not SSH (rpi-connect, web terminals) are not detected: use the manual `sudo nmcli device disconnect wlan0` there. The refusal names the ways on: re-run over the cable LAN, or disconnect the client by hand (`sudo nmcli device disconnect wlan0`), re-run without the option and reconnect it afterwards.
3. System remembers the client connection (its UUID) before any change, disconnects it with `nmcli device disconnect wlan0` (which also stops NM's autoconnect from taking the radio back) just before the self-test, and runs the self-test with the cable in.
4. System reconnects the same connection (`nmcli connection up uuid`) right after the self-test, whether it passed or failed, and from the setup's EXIT trap for any other exit or signal. When the reconnect fails the setup exits non-zero (also after a passing self-test, when the fallback is armed) and prints the command to run.
5. Use case ends. With the option but no client up, the setup says so and proceeds unchanged.

### Outcome (was UC-061 postconditions)

Success:

- With the uplink not connected and no Wi-Fi client on `wlan0`, the hotspot is up and the Host reaches `http://10.42.0.1/`; with the uplink stably connected, the hotspot is down.
- The status file names the state (`on`, `off`, `waiting`, `failed`, `disabled`, `not-set-up`) and its reason, with no secret.

Failure:

- The hotspot is down, the reason is in the journal and the status file, and the game stack is unaffected.

### Rules

#### UC-061 BR-001: Fallback only

The hotspot comes up only as the fallback of step 2, never on demand, by flag, pad or command (maintainer decision 2026-10-24). The one exception is the setup's forced self-test (FR-186, Pre-3): it runs only while the uplink is connected, holds the hotspot for at most 30 s and takes it down again.

#### UC-061 BR-002: No internet sharing

The hotspot has no upstream, for two reasons: the check brings it up only while neither the uplink nor a Wi-Fi client is connected (and on one radio a client and the access point never run together; there is no second adapter, maintainer decision 2026-10-24), and Docker sets the host's `FORWARD` policy to `DROP`, which also drops NM's shared-mode NAT towards an uplink. The check never brings the hotspot up while the uplink is connected, even before that connection is stable; the only overlap is the 15 s of A1. NM's masquerade rule also rewrites a hotspot client's address towards the Docker bridge, so envoy's access log shows the bridge gateway, not the phone.

#### UC-061 BR-003: 5 GHz only

The access point uses a non-DFS 5 GHz channel: 36, 40, 44 or 48 (the Pi has no radar detection in access point mode). Never 2.4 GHz, also not as a fallback: the controllers' Bluetooth uses 2.4 GHz.

#### UC-061 BR-004: Password and SSID handling

The password is 8 to 63 characters of printable ASCII without space and backslash (0x21 to 0x7E, no `\`), so the NM keyfile stores it verbatim. It is stored only in the root-owned 0600 NM keyfile; JoustMania never writes it anywhere else: not in git, `.env`, `.env.local` (compose loads it into containers), a command line, a log, the journal, the status output or the dashboard. NM itself may disclose it to root and to privileged D-Bus clients (the pairing daemon's container runs as root with the system D-Bus socket). The setup reads it with `IFS= read -rs` twice from the terminal, from the first line of a regular 0600 file owned by the caller (`--psk-file`, deleted afterwards), or from one line on stdin (`--psk-stdin`); it passes it to `render` through a pipe from a builtin and, after `nmcli connection load`, compares the stored password with the entered one through pipes (`cmp`), never printing either. The helper and the setup refuse to run with shell tracing (`bash -x`) on. The SSID is 1 to 32 characters of `A-Z a-z 0-9 _ -` and space, with no leading or trailing space.

#### UC-061 BR-005: Access control

The WPA password is the only access control. On the hotspot, `/flags`, `ProcessInput` and `ForceEndGame` stay unauthenticated, as on the LAN (#2050 D11).

#### UC-061 BR-006: Independent of the game

The hotspot unit, timer and hook never order against, gate or fail `joustmania.service`, and the game never waits on them. A decision the helper could not carry out is logged and recorded, and the helper still exits 0. An upgrade or rollback of the checkout (UC-060 A2, A3) never touches the network setup: the root-owned installed copies stay as they are until the setup runs again.

#### UC-061 BR-007: Status line is INFO only

The hotspot line of `joustmania-status` is information, like the clock line: it never changes `state:` or the exit code, and the hotspot is never a UC-060 state (UC-060 BR-005; maintainer decision D3). Its only effect on the ACT LED is the hotspot cue of BR-011 (#2137), which the `led:` line names.

#### UC-061 BR-008: When the uplink counts as connected

The uplink (`uplink_iface`, default `eth0`) is connected when its NM device state is 100 and it has an IPv4 address outside 169.254.0.0/16; states 40 to 90 mean it is still connecting; anything else, a missing device included, means not connected (maintainer decision D1). The check records since when the uplink has not been stably connected and since when it has been connected, in `/run/joustmania-hotspot/` (cleared at boot), on the seconds-since-boot clock (`/proc/uptime`), so a wall-clock step (NTP) never stretches or skips a wait: up after 20 s not connected (60 s while connecting or briefly connected), down after 15 s connected without a break, nothing in the first 45 s after boot. Defaults; `hotspot.conf` may change them within fixed bounds.

#### UC-061 BR-009: Phones without internet

There is no captive-portal workaround (maintainer decision 2026-10-27). A phone may keep mobile data as its default network on a Wi-Fi without internet, so the Host instructions say: turn mobile data off, or confirm "stay connected" when asked.

#### UC-061 BR-010: Configuration is host bootstrap

The non-secret settings live in `/etc/joustmania/hotspot.conf` (`enabled`, `ssid`, `channel`, `uplink_iface` and the BR-008 delays). Root parses it as `key=value` lines against an allow-list and never sources it; an unknown key or an invalid value is logged and ignored (the default applies). The password lives only in the NM keyfile (BR-004). Neither is a flagd flag or a compose environment value (C-045).

#### UC-061 BR-011: One LED owner, fixed precedence

The UC-060 LED helper (`/usr/local/lib/joustmania/joustmania-led`, root-owned) is the only writer of the ACT LED; the hotspot helper only asks it to set or clear the cue (`hotspot-failed`, `hotspot-clear`). The helper keeps the UC-060 state (`/run/joustmania-led/state`) and the cue (`/run/joustmania-led/hotspot`) as two records, takes a lock for every change (the game unit's hooks and the hotspot timer can run at the same moment; it waits at most 10 s for the lock, then goes on without it) and writes the pattern that wins, highest first:

| Rank | Shown when | Trigger | Pattern |
|---|---|---|---|
| 1 | UC-060 `failed` | `timer` 100/100 ms | 5 Hz blink |
| 2 | UC-060 `starting` | `heartbeat` | double flash, load dependent |
| 3 | hotspot cue (A10) | `timer` 200/1800 ms | one short flash every 2 s |
| 4 | UC-060 `degraded` | `timer` 500/500 ms | 1 Hz blink |
| 5 | UC-060 `ok` | `default-on` | solid on |
| 6 | no state, or a clean stop of the game unit | the board's own (saved) trigger, usually `mmc0` / `actpwr` | disk or power activity |

A failed game outranks access (the game is the product, and the Host needs the keyboard or the cable either way); `starting` is short and is judged again at the end of the start; the cue outranks `degraded` and `ok` because without the hotspot the Host has no other way to learn that the network side failed. The cue is no UC-060 state: `joustmania-status` keeps `state:` and the exit code from UC-060 alone (BR-007).

### Code, tests and limits

**Code / tests:** `scripts/setup/joustmania-hotspot.nmconnection.template` (the profile: fixed UUID, `type=wifi`, `interface-name=wlan0`, `autoconnect=false`, `mode=ap`, `band=a`, WPA2-PSK CCMP, `pmf=1` (disabled), `powersave=2`, `ipv4.method=shared` at 10.42.0.1/24 with `never-default`, IPv6 off); `scripts/setup/joustmania-hotspot.sh` (installed root-owned as `/usr/local/lib/joustmania/joustmania-hotspot`, the #2130 pattern: `evaluate` main steps 1 to 3 and A1 to A7, `status`, `render` (BR-004: reads the password from stdin, never from argv; no pattern substitution), `selftest` (BR-001 exception), `remove`); `scripts/setup/joustmania-hotspot.service` (`Type=oneshot`, `StartLimitIntervalSec=0`, `TimeoutStartSec=240`, `KillMode=mixed`, no `[Install]`: only the timer and the hook start it, so boot never waits on it; no reference to `joustmania.service`); `scripts/setup/joustmania-hotspot.timer` (`OnBootSec=30s`, `OnUnitActiveSec=60s`); `scripts/setup/90-joustmania-hotspot` (NM dispatcher hook: `systemctl start --no-block`, never a restart). `scripts/setup/install_hotspot.sh` (FR-186: pre-checks that change nothing on a refusal (NetworkManager, `wlan0`, Wi-Fi country set and persisted in `cmdline.txt`, the channel present and not disabled or no IR, hard block, `dnsmasq-base` installed or refused, the full dnsmasq service, other `:53` / `:67` listeners, ufw and firewalld with the exact allow rules and `--firewall-rules-added`), the radio switched on, root-owned copies, the keyfile through a hidden temp name, an atomic move and `nmcli connection load`, the `net.ipv4.ip_forward=1` drop-in `/etc/sysctl.d/90-joustmania-ip-forward.conf` so the hotspot going down never leaves Docker without forwarding, the configuration written with `enabled=no` until the forced self-test passes, and put back to `enabled=no` by every later refusal or interruption, so a refused setup never leaves a fallback that the hook could start (except over the hotspot, A8: decided once before any change, the fallback stays `enabled=yes` throughout and a refused or interrupted run restores the previous profile, so the Pi never loses its only access); the stored SSID and channel checked after the load, `--remove` through the installed remover; A11: `--pause-wifi-client` / the terminal prompt, the wlan0 client UUID read before any change, `nmcli device disconnect wlan0` before and `nmcli connection up uuid` after the self-test and in the EXIT trap, a non-zero exit when that fails, `session_over_wlan`: a fail-closed refusal from the ancestors' `SSH_CONNECTION`, `ss` sshd peers and `ip route get`); `joustmania_hotspot_hint` in `scripts/setup/lib-joustmania.sh` (after an upgrade or rollback it prints "hotspot files changed" when the installed copies differ from the checkout, or names the installed remover when the checkout has no setup, BR-006). A10, BR-011: the helper's `led_cue` asks the installed LED helper (stderr dropped, `timeout 15`) only on a change into or out of `failed`, recorded as `led_cue` in its state dir only when the helper answers 0, so a failed call is retried by the next check and logged once (`led_error`); `remove` disarms first (deletes the dispatcher hook and the configuration, then waits for a running check), so no hook run during `remove` can start the unit again or set the cue (#2196), then takes the hotspot down and clears a set cue. `scripts/setup/joustmania-led.sh`: `hotspot-failed` / `hotspot-clear` only change the record; the precedence lives in `effective`, and `render` applies it for every state change, restore and cue change. Tests: `scripts/tests/test-install-hotspot.sh` (stubs only), `scripts/tests/test-joustmania-hotspot.sh` (stub `nmcli`, `iw`, `systemctl`, fake time; A10 with the real LED helper on a fake sysfs), `scripts/tests/test-joustmania-led.sh` (BR-011: every rank against the cue, set and clear; a change waits while another holder has the lock, #2135), `scripts/tests/test-joustmania-status.sh` (the cue on the `led:` line, BR-007), `tests/contract/test_hotspot_profile.py` (template, unit and timer, GKeyFile round trip of hostile passwords).

**Bluetooth:** the stage rig disables the Pi's internal Bluetooth (`dtoverlay=disable-bt` from `setup_host.sh`) and uses USB dongles, so a 5 GHz access point shares neither the 2.4 GHz air nor the antenna; the remaining costs are CPU, interrupts and power (#2089: AP mode draws more current, and the dongles brown out first). A rig that keeps the internal Bluetooth on time-slices Wi-Fi and Bluetooth on one chip even at 5 GHz; NFR-008 covers the stage rig only.

**On-Pi checklist (#2088), closes NFR-008 and NFR-028:** pull the cable (up within 60 s) and plug it back (down within 30 s); boot without a cable, with a known Wi-Fi profile in range and out of range; boot with a cable and slow or no DHCP; a link flapping every 10 s for 2 minutes; `nmcli connection down` of the hotspot by hand (A7); a known Wi-Fi client that drops (phone tether off and on again within 20 s: no hotspot, the client reconnects; off for longer: the hotspot comes up, A2); join from an iPhone and an Android phone, mobile data on and off; dashboard, Jaeger and Grafana load; `systemctl restart docker` while the hotspot is up, then reload the dashboard; `sysctl net.ipv4.ip_forward` after the cable returns (NM must not reset what Docker needs); `iw` as `pi`; the regulatory domain after a reboot, before the hotspot starts; whether PMF optional and WPA3-SAE work in access point mode on brcmfmac; setup and password change over the hotspot (A8); upgrade and rollback across the #2099 boundary; report-gap p95 of 7 controllers with the hotspot off, then on with one client streaming the dashboard, on the same power supply (NFR-008).

**Limits:** the brcmfmac firmware serves about 8 clients, enough for the Host and the crew, not for the audience. Only 4 channels are allowed (BR-003); a crowded venue band slows the dashboard, not the game (the dashboard is not on the game path). A host firewall with default deny (ufw, firewalld) would block DHCP, DNS, HTTP and SSH on `wlan0`: the setup refuses it or prints the allow rules (FR-186).

**Later extension (not in these flows):** a voice or LED announcement of "hotspot on" through the menu.

## 7. Acceptance checklist

Each item is a property of the stage host and the tests that check it. "On-Pi" items need the real Pi and are checked
by hand (#1402, #2088); everything else runs in CI on stubs, a fake sysfs and fake time (`scripts/tests/`,
`tests/contract/`).

| Id | Property | Former ids | Checked by |
|---|---|---|---|
| SH-1 | A boot with no network, or with no new image, starts the pinned version from the images on the Pi, pulls nothing, recreates 0 containers and removes none that ran before; a missing image is named with its recovery command and only what exists starts (FR-180, NFR-026) | UC-060 main steps 1 to 3, BR-001, BR-002, Pre-1, Pre-2, Post-S-1, Post-S-2, Post-F-1 | `scripts/tests/test-pi-start-offline.sh`, `scripts/tests/test-pi-start-safe.sh`, `scripts/tests/test-pi-start-compose-files.sh`, `tests/contract/test_joustmania_unit.py` |
| SH-2 | The game services start and are judged before observability; observability that does not come up leaves the game running and the state `degraded` | UC-060 main steps 3 and 4, A1, BR-004 | `scripts/tests/test-pi-start-offline.sh`, `tests/contract/test_compose_startup.py` |
| SH-3 | Game services that do not start make the state `failed`; systemd retries at most 3 times in 15 minutes | UC-060 A5 | `scripts/tests/test-pi-start-safe.sh`, `scripts/tests/test-pi-start-offline.sh`, `tests/contract/test_joustmania_unit.py` |
| SH-4 | A branch checkout starts nothing and names the pin command; a checkout git cannot read fails open as `degraded` | UC-060 A4 | `scripts/tests/test-pi-start-offline.sh`, `scripts/tests/test-joustmania-status.sh`, `scripts/tests/test-joustmania-upgrade.sh`, `scripts/tests/test-setup.sh` |
| SH-5 | The upgrade refuses `latest`, a branch and a target below `MIN_PINNABLE`, pulls before it switches, derives `IMAGE_TAG` from the checkout and prunes only after the new version is healthy (FR-182, C-040) | UC-060 A2, BR-003 | `scripts/tests/test-joustmania-upgrade.sh` |
| SH-6 | An unhealthy new version is rolled back automatically, `--rollback` works offline with the local images, `--no-rollback` leaves it for inspection (FR-182) | UC-060 A3 | `scripts/tests/test-joustmania-upgrade.sh` |
| SH-7 | Operator restarts reset the start limit first; a start systemd still refuses is reported as refused, never rolled back | UC-060 BR-006 | `scripts/tests/test-joustmania-upgrade.sh`, `scripts/tests/test-joustmania-status.sh` |
| SH-8 | `joustmania-status` and the ACT LED use one vocabulary (`starting`, `ok`, `degraded`, `failed`); the LED is applied at the end of the start, restored on a clean stop, and written only by the root-owned helper (C-042) | UC-060 main step 5, BR-005, Post-S-3, Post-S-4 | `scripts/tests/test-joustmania-status.sh`, `scripts/tests/test-joustmania-led.sh`, `tests/contract/test_joustmania_unit.py` |
| SH-9 | `./setup.sh` on a fresh clone installs the runtime, `.env.local` (mode 600), the autostart, the pin and the images, and exits non-zero when a step fails | UC-060 A6 | `scripts/tests/test-setup.sh`, `scripts/tests/test-setup-scripts.sh`, `scripts/tests/test-docker-log-defaults.sh` |
| SH-10 | The development setup runs its nine steps in order with root only through sudo; `update_permissions.sh` repairs the checkout owner | UC-060 A7 | `scripts/tests/test-setup-scripts.sh` |
| SH-11 | The hotspot comes up only as the fallback when the uplink is not connected and no Wi-Fi client is up or connecting, after its delays, and goes down after 15 s of stable uplink; flaps, carrier without address and a drop while in use follow the recorded state (FR-184, NFR-028) | UC-061 main steps 1 to 3, A1, A2, A4, A6, A7, A9, BR-001, BR-008, Pre-1 | `scripts/tests/test-joustmania-hotspot.sh`, `tests/contract/test_hotspot_profile.py` |
| SH-12 | A hotspot that cannot start, or is not set up or switched off, is recorded with its reason; a failed hotspot shows the ACT LED cue at its fixed precedence | UC-061 A3, A5, A10, BR-011 | `scripts/tests/test-joustmania-hotspot.sh`, `scripts/tests/test-joustmania-led.sh`, `scripts/tests/test-joustmania-status.sh` |
| SH-13 | The profile is 5 GHz only on a non-DFS channel, WPA2-PSK CCMP, shares no internet, and the password is stored only in the root-owned keyfile and never printed, and it is the only access control on the hotspot; the settings are host bootstrap, not flags (C-043, C-044, C-045) | UC-061 BR-002, BR-003, BR-004, BR-005, BR-010 | `tests/contract/test_hotspot_profile.py`, `scripts/tests/test-install-hotspot.sh`, `scripts/tests/test-joustmania-hotspot.sh` |
| SH-14 | The setup checks before it changes anything, proves the access point with a forced self-test, keeps the fallback armed and the old profile when re-run over the hotspot, pauses and restores a Wi-Fi client with `--pause-wifi-client`, and undoes it all with `--remove` | UC-061 A8, A11, BR-001 (self-test), BR-004, Pre-2, Pre-3 | `scripts/tests/test-install-hotspot.sh` |
| SH-15 | The hotspot never orders against, gates or fails the game unit, and its status line is INFO only; it works without the start of §1 installed, while convention play needs that offline start (FR-180) | UC-061 BR-006, BR-007, Pre-4 | `tests/contract/test_hotspot_profile.py`, `scripts/tests/test-joustmania-status.sh`, `scripts/tests/test-joustmania-hotspot.sh` |
| SH-17 | The upgrade and the rollback never write host files; a missing or differing operator pad udev rule is named with its fix after a healthy upgrade, after `--rollback` and on an INFO status line that never changes the state (#2858) | UC-060 A2, A3, BR-005 | `scripts/tests/test-joustmania-upgrade.sh`, `scripts/tests/test-joustmania-status.sh` |
| SH-16 | On-Pi: start-up time N (NFR-027), the LED and the start on the Pi (#1402); the dashboard, Jaeger, Grafana and SSH at `10.42.0.1` over the hotspot in convention play mode (was FR-185), the hotspot timings (NFR-028) and the controllers' report gaps with the hotspot on (NFR-008), per the on-Pi checklist of §6 (#2088) | UC-061 main steps 4 to 6, BR-009 | manual (#1402, #2088) |
