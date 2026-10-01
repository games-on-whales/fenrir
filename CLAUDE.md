# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

- Kubernetes orchestration for multiple [Wolf](https://github.com/games-on-whales/wolf) game-streaming
  instances ("direwolf"). README says it is not yet usable; expect POC-grade code.
- Go module path is `games-on-whales.github.io/direwolf` (not the GitHub repo name `fenrir`).
- CRD API group: `direwolf.games-on-whales.github.io/v1alpha1` — kinds `App`, `User`, `Session`, `Pairing`.

## Commands

- Test (matches CI): `make test` → `go test -race -shuffle=on -timeout 5m ./...`
- Single test: `go test -race -run TestName ./pkg/controllers/`
- Lint / format: `make lint`, `make fmt` (golangci-lint v2, config in `.golangci.yml`); `make vet`.
- Build one binary: `go build ./cmd/<operator|moonlight-proxy|wolf-agent>`
- Image: `docker build --build-arg APP_NAME=<cmd name> .` — one Dockerfile, `APP_NAME` picks the cmd.
- Non-Go images live in `images/<name>/` (own Dockerfile + context), e.g. `images/library`
  (linuxserver/steam + Heroic), `images/retroarch` (GoW RetroArch + pinned cores).
  CI builds them via matrix `include:` entries carrying `context:`.
- Codegen after editing `pkg/api/v1alpha1/*.go`: `hack/update-codegen.sh` (bash + python3).
  - Regenerates `zz_generated.*`, `pkg/generated/` (clientset/listers/informers/applyconfig),
    `crds/`, and `schemas/`. All are committed — never hand-edit them.
  - Keep `crd:generateEmbeddedObjectMeta=true`: without it an embedded template's `metadata` is a
    bare `{type: object}` and the API server prunes its labels/annotations on admission (XERK-1347).
    Unit tests seed fake clients that never prune; `TestCRDsKeepEmbeddedMetadata` guards it.
- Chart: `charts/direwolf-operator` has no `crds/` dir in git; CI copies `crds/*.yaml` in before
  `helm lint --strict` / `helm template --kube-version 1.36.0` (kubeVersion floor is >=1.28).

## Architecture

Three binaries in `cmd/`, all sharing `pkg/`:

- **moonlight-proxy** (`pkg/moonlight`): the Moonlight HTTP/HTTPS server users connect to.
  - Pairing creates a `Pairing` CR mapping client-cert fingerprint → `User`; HTTPS requests are
    authorized by fingerprint lookup.
  - The PIN is entered on the pairing page (`pkg/moonlight/pinpage.go`, own `--pin-port`) behind
    Authentik; the username header is trusted only from `--pin-trusted-proxies` peers that also
    send the `--pin-proxy-secret-file` secret.
    - Never serve it on the Moonlight ports: those are on the LoadBalancer, where SNAT can make
      an internet client look like a trusted in-cluster peer.
  - App list is rendered from `App` CRs.
  - `/launch` creates a `Session` CR, then blocks until the operator writes an RTSP URL into
    `Session.status` (bounded by `--launch-timeout` and the client connection).
  - `/resume` must not replace the Session (that deletes the pod the game runs in): it writes the
    client's new keys into its `spec.config` and waits for `status.attachedGeneration` to catch up.
  - `/cancel` deletes the user's Sessions at once, bypassing the disconnect grace period.
- **operator** (`pkg/controllers/session.go`): leader-elected (lease); only `SessionController` runs.
  - Per `Session` it creates one bare pod (game container + wolf + wolf-agent + pulseaudio
    sidecars, sharing `XDG_RUNTIME_DIR`) and PVCs. No Service: pods use `hostNetwork` on the node
    Moonlight clients stream from (`--session-node-selector`), since Moonlight only accepts a port
    redirect.
  - The pod is Job-like: named after, and controlled by, its Session; `restartPolicy: Never`.
    - Any exited container, or the pod gone (`PodCreated` condition True, no pod), ends the
      Session; deleting it GCs the pod and the pod's generated ResourceClaims. Never recreate it.
    - Disconnects are only seen by polling wolf-agent (`streamPollInterval`): wolf-agent stops
      Wolf's session on pause and has no Kubernetes access.
    - A disconnect sets `status.disconnectedAt`; the pod is kept for `--disconnect-grace-period`
      (default 10m) for `/resume`, then the Session is deleted. Tests: `session_lifecycle_test.go`.
    - The unstarted-session reaper must skip disconnected sessions (`expiredReason`).
  - Every pod listener (Wolf HTTP/HTTPS too, wolf-agent) must come from the session's port block
    (`pkg/controllers/ports.go`, `--session-port-range`), or pods on the node collide.
    - Blocks are keyed by pod and freed when its Session is deleted; `status.ports` is the
      record, replayed via `Claim` on operator start. Tests: `ports_test.go`.
    - Ports stay declared as containerPorts: under hostNetwork they become hostPorts, so the
      scheduler holds a pod whose block a terminating predecessor still binds.
    - A pod records its block in the `port-block` annotation; one whose block differs from
      `status.ports` ends the Session, since the advertised ports would go unserved.
    - Exception: Wolf's mDNS (UDP 5353, SO_REUSEPORT) is hardcoded and outside the block.
  - Chart: moonlight-proxy is host-networked with a nodeSelector and tolerations that must match
    the operator's `--session-node-selector` / `--session-tolerations`.
    - Talos labels `kubernetes.io/hostname` with the FQDN (`talos04.xerktech.com`), and talos04
      is tainted `nvidia.com/gpu=present:NoSchedule`; miss either and every pod stays Pending.
  - Gateway API code in `session.go` is commented-out experimentation.
  - `LibraryController` (`pkg/controllers/library*.go`, off unless `--library-port`) starts the
    Steam + Heroic Library pod on a page visit, proxies it, and stops it when idle.
    It and game sessions never run together (shared Steam home); see `.claude/rules/library.md`.
  - The catalogue (`pkg/controllers/catalogue.go`, off unless `--catalogue-*-app`) scans the
    Library pod for Steam/Heroic installs and keeps one App per game; see `.claude/rules/catalogue.md`.
  - RomM sync (`pkg/controllers/romm.go`, off unless `--romm-url`) keeps one RetroArch App per
    playable RomM ROM, via the catalogue's App sync; see `.claude/rules/romm.md`.
  - `App.spec.gpu` becomes a per-Session DRA ResourceClaim (`pkg/controllers/gpu_claim.go`).
  - Watches session pods (label `direwolf/session=true` only) to re-reconcile their Session.
- **wolf-agent** (`pkg/controllers/agent.go`, `pkg/wolfapi`, `pkg/fakeudev`): sidecar talking to
  Wolf's HTTP API over a mounted unix socket.
  - Syncs desired sessions into Wolf and emulates udev (mknods `/dev/input/*` nodes and writes
    `/run/udev/data`, emptyDirs shared with the game container) so SDL/Steam see hotplugged
    controllers. Rationale and Talos test: `.claude/rules/input-hotplug.md`.
  - Its `/api/v1/` proxy drives Wolf (can run arbitrary containers) and, under hostNetwork, is
    reachable on the node IP. It requires a per-Session bearer token (Secret
    `<session>-wolf-agent-token`, `pkg/controllers/agent_token.go`); the operator dials the pod IP.
    Wolf's API stays on the socket.
  - The same Secret holds wolf-agent's per-session serving cert, which the operator pins
    (RootCAs + ServerName `wolf-agent`). Never dial it with InsecureSkipVerify: other
    host-networked processes can bind the agent port and would collect the token.
  - wolf-agent re-reads its token and its cert on every request/handshake: the operator may
    regenerate the Secret under a running pod and uses the new token and cert at once.
  - `fakeudev` is Linux-only for real work (`fakeudev_linux.go` vs `fakeudev_other.go` stub);
    tests touching it behave differently on Windows/macOS.
- **`pkg/generic`**: typed generic wrappers over client-go informers/listers plus a reusable
  `Controller[T]` reconcile loop. New controllers should build on it, not raw `cache.SharedIndexInformer`.
- Game containers must wait for the `WAYLAND_DISPLAY` socket before starting, or the pod never goes
  Ready and the stream is never started.

## CI (`.github/workflows`)

- PRs: `go test -race`, govulncheck, golangci-lint, Docker build of all three images, and chart
  lint/template/package.
- Push to main / release (`builder.yml`): pushes images to `ghcr.io/<owner>/fenrir/*` (operator image
  is named `direwolf-operator`), then rewrites chart `values.yaml` image refs to digests and pushes
  the chart as OCI.
  - Pass `github.ref*` into scripts via `env:`, never `${{ }}` splices — tag names are untrusted.
