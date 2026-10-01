---
paths:
  - pkg/fakeudev/**
  - pkg/controllers/agent.go
  - pkg/controllers/session.go
  - pkg/controllers/session_test.go
  - cmd/nri-input/**
---

# Controller hotplug into the game container

- Path chosen: wolf-agent mknods each `/dev/input/*` node into an emptyDir mounted at `/dev/input`
  in wolf-agent and every App container, and writes `/run/udev/data` into a second shared emptyDir.
  - Don't switch to a hostPath `/dev/input`: it hands the game every host input device.
- Measured on Talos v1.13.8 / containerd 2.2.6 (XERK-1303, a busybox pod on talos03):
  - The emptyDir mounts `rw,relatime`, not `nodev`: a node mknod'ed by one container opens from a
    sibling running non-root with all caps dropped.
  - Opening is gated only by the device cgroup, by number and not path: 13:64 gave EPERM without
    the `cmd/nri-input` grant (13:* rwm). Re-check `nodev` if Talos or kubelet changes.
- wolf-agent needs CAP_MKNOD (runtime default; dropping ALL in its sidecar policy breaks hotplug).
- Only char major 13 under `/dev/input/` is created (`CreateDeviceNode`); a Wolf event can't make
  a disk node. `/dev/hidraw*` is not handled: pads are uinput Xbox pads (no uhid on Talos).
- An App that mounts its own `/dev/input` keeps it (`withHotplugMounts` skips taken paths).
  A wolfAgent sidecar policy may not mount there (`validateNoHotplugOverride`).
  - Compare mounts via `mountTarget`: the apiserver accepts relative mountPaths (`run/udev/data`),
    which the runtime resolves against `/`; `/var/run` (Alpine symlink) is reserved as an alias.
  - It guards misconfiguration only: a User can already mount hostPath into sidecars.
  - Keep the hotplug mounts appended after policy mounts: a policy mount at `/var/run` or `/run`
    only loses to `/run/udev` because it is mounted first.
- Wolf sends no unplug when a stream stops: it destroys the devices on StopStream. The agent
  clears both dirs on pause (`clearDevices`), or a stale 0666 node would open whatever device the
  kernel gives that minor next (another session's keyboard). Tests: `TestAgentPauseClearsDevices`.
- The dirs are writable by the app container, which can swap a node for a symlink at any moment:
  never chmod/chown by path after mknod (`setNodeMode` goes through an O_PATH fd).
  Tests: `TestSetNodeModeRefusesSwappedNode`.
- CI runs tests non-root, so mknod EPERMs there: logic tests stub `makeCharNode`, or a refusal
  test passes for the wrong reason.
- The udev netlink send (`SendEvent`) gets EPERM under hostNetwork without CAP_NET_ADMIN on the
  host netns, and with it reaches every hostNetwork pod on the node (XERK-1337).
- Pod-spec tests: `TestSessionPodSharesHotplugVolumes`, `TestSessionPodKeepsAppsOwnInputMount`
  (`pkg/controllers/session_test.go`); `TestCreateAndRemoveDeviceNode` (`pkg/fakeudev`).
