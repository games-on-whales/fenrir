# fenrir
The leader of the pack - control multiple distributed instances of Wolf in K8S

NOTE: NOT IN A USEABLE STATE!

## High Level Idea

![Proof-of-Concept Diagram that is likely outdated](poc.svg)
> Proof-of-Concept Diagram that is likely outdated

Wolf is designed for a single machine with multiple users. But when wolves come
together into a cluster they form a pack. Every pack needs a leader to coordinate
the other wolves.

This project currently has three components:

- `moonlight-proxy`
- `wolf-agent`
- `operator`

## moonlight-proxy

Moonlight proxy is the entrypoint for the user's moonlight requests. It hosts
a standard Moonlight HTTP/HTTPS server to handle pairing of users, and the
starting/stopping of games.

### Authentication/Authorization

To manage user sessions moonlight-proxy creates an instance of a CRD `Pairing`.
`Pairing` contains the association between the Moonlight client's certificate
and the user they are authenticated against.

When calls to HTTPS endpoint are made, `moonlight-proxy` calculates the fingerprint
of the client certificate, and looks up a matching `Pairing` to find the user that
is currently authenticated.

#### PIN Codes

While Moonlight shows its PIN, the pair request waits on the pairing page
(`/pin/` on `--pin-port`). That page is meant to sit behind an Authentik proxy
outpost: the outpost authenticates the user and sets `--pin-user-header`
(default `X-Authentik-Username`), whose value must be the name of a `User` in the
proxy's namespace. The `Pairing` created for the client belongs to that `User`.

- The header is honoured only when the TCP peer is in `--pin-trusted-proxies`;
  any other peer gets a 403. `X-Forwarded-For` is ignored.
- Every request must also carry `X-Direwolf-Proxy-Secret` equal to the contents of
  `--pin-proxy-secret-file` (at least 32 bytes), or it gets a 403. Set it on the
  Authentik provider with a property mapping returning
  `{"ak_proxy": {"user_attributes": {"additionalHeaders": {...}}}}`. The source
  check alone is not enough when the proxy is host-networked: traffic from the
  outpost arrives masqueraded to its node's address, which every pod on that
  node shares.
- The page has its own listener and is not served on the Moonlight ports, so it
  must not be put on the LoadBalancer Service. Point the outpost at a ClusterIP
  Service for `--pin-port`, and set `--pin-trusted-proxies` to the outpost pods'
  range only.
- Each pending request accepts one PIN; a wrong PIN fails the handshake.

### App Lists

Apps are stored in a CRD on Kubernetes called `App`. Moonlight-proxy renders
the list as XML to return to Moonlight client to present to the user.

### Orchestration

In response to `/launch` or `/resume` from Moonlight client launching an app,
`moonlight-proxy` creates a `Session` object which associates `User`, `Game`,
and `Pairing` for the specific client used. It then waits for the `Session`
to have an `rtsp` URL added to its status by the `operator` to hand back to the uses.

### Configuration

`moonlight-proxy` is configured via command-line flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--port` | `47989` | HTTP port to listen on. |
| `--secure-port` | `47984` | HTTPS port to listen on. |
| `--tls-cert` | `server.crt` | Path to the server TLS certificate. |
| `--tls-key` | `server.key` | Path to the server TLS key. |
| `--namespace` | `$POD_NAMESPACE` | Namespace to watch for CRDs. |
| `--pin-port` | `0` (off) | Port for the pairing page. Requires `--pin-trusted-proxies` and `--pin-proxy-secret-file`. |
| `--pin-proxy-secret-file` | | File holding the secret the proxy must send in `X-Direwolf-Proxy-Secret` (surrounding whitespace trimmed, at least 32 bytes). |
| `--pin-trusted-proxies` | | Comma-separated CIDRs of the proxy (Authentik outpost) allowed to assert the user. |
| `--pin-user-header` | `X-Authentik-Username` | Header carrying the authenticated username; must equal a `User` name. |
| `--launch-timeout` | `60s` | How long `/launch` waits for the operator to create a session and expose its stream URL before giving up. The wait is still bounded by the client connection, so a disconnecting Moonlight client cancels it early. Raise this if cold starts (image pull + wolf boot + `wolf-agent` readiness) are getting cancelled with an HTTP 500; lower it to fail faster. |

## wolf-agent

Wolf-Agent is a sidecar container with Wolf's HTTP API socket mounted locally.
It is responsible for synchronizing the intended list of sessions in Kubernetes
with the currently active sessions in wolf, as well as calling `fake-udev` for
controller support.

Wolf's API is only ever served on its unix socket inside the pod. wolf-agent
proxies it over HTTPS on `--port` (the session's port block) and rejects every
`/api/v1/` request without `Authorization: Bearer <token>`; `/readyz` and
`/livez` stay open for kubelet probes.

| Flag | Default | Description |
|---|---|---|
| `--token-file` | _(required)_ | File holding the bearer token. The operator generates one per session Deployment (Secret `<user>-<app>-wolf-agent-token`) and mounts it. wolf-agent refuses to start without it. |

The wolf-agent port is not published on the session `Service`; the operator
dials the pod IP directly.

## Operator

The operator is a component which runs a leader election and control loops
for all of the CRDs used for Wolf. The only really interesting one is `Session`.
The loop for `Session` is responsible for setting up port forwards, PVCs,
deployments, and more.

In the future the operator will also expose a subset of Wolf API for WolfManager
to communicate with (to manage lists of apps, for instance). This implementation
will simply create k8s api objects.

Only one operator is running the writable control loop at a time. Only 2-3 replicas
are ever needed just for high availability. A single active operator is likely 
enough to handle all the sessions created by a single IP (as it is only in the loop for creation/deletion)

### Pod Layout

Each pod spawned by `operator` has multiple containers:

- User's Game Container
- Wolf-Agent
- Wolf
- Pulseaudio

Each of these at least have a common `XDG_RUNTIME_DIR` as well as some other
specific mounts for each of them.

User's Game Container MUST have a pre-launch script that waits for the `WAYLAND_DISPLAY`
socket to be created, otherwise the container will be in a crashloop, and the
Pod will never be `READY` and the stream is never started.

### Port Forwarding

Moonlight streams from the same host it paired with, and can only be redirected
to a different *port*. So session pods run with `hostNetwork` on one node (pinned
with the operator's `--session-node-selector`, e.g.
`kubernetes.io/hostname=talos04.xerktech.com`; Talos nodes carry their FQDN in
that label) and share that node's IP with `moonlight-proxy`, which must also be reachable on
it at 47984/47989 (e.g. a host-networked proxy pinned to the same node).

For each session Deployment the operator allocates a block of 7 host ports from
`--session-port-range` (default `20000-20999`, below the ephemeral and NodePort ranges): Wolf HTTP, HTTPS, RTSP, control
(ENet), video, audio, and wolf-agent. The block is recorded in `Session.status.ports`,
Wolf is started with the matching `WOLF_*_PORT` variables, and the RTSP URL returned
by `/launch` carries the session's RTSP port. The block is freed once no `Session`
uses that Deployment. Firewalls between clients and the node must allow the range.

The chart runs `moonlight-proxy` host-networked with the same node selector and
tolerations as the operator's `--session-node-selector` and `--session-tolerations`
(talos04 carries `nvidia.com/gpu=present:NoSchedule`); keep them in sync. Host networking needs the
release namespace to allow it (PodSecurity `privileged`). App templates must not
declare their own container ports: on the host network they become host ports and
collide between sessions. Wolf's mDNS responder (UDP 5353, shared via SO_REUSEPORT)
is outside the block and advertises the node on the LAN at 47989, the proxy's port.

#### Alternative Design Idea: Gateway API

Rather than special cilium load balancer annotation, a better method would be
to create `UDPRoute` and `TCPRoute` objects from https://gateway-api.sigs.k8s.io

This is a general purpose port-forwarding API for Kubernetes from Public Virtual
IPs. It is a great alternative to special LB sharing annotations.

#### Alternative Design Idea: Full Dumb Relay

I didn't attempt to implement a relay. Maybe someone can try it so we have a
general purpose implementation "that just works" for everyone.

#### Alternative Design Idea: Implement RSTP + RTP PING, Use Relay for ENET

This idea requires some method of masquerading Egress traffic from `wolf` pods.
Cilium has an easy way to do this, but other methods may not.

The basic idea would be that we dont need to forward any ports if we implement
the RTSP handshake, RTP PING in `moonlight-proxy`. Once direwolf gets the
IP/PORT of the moonlight client it can have `wolf` just start uploading a stream,
as long as it can fake the source IP of the packets from `wolf`.

## Getting started:
Before you start you should install metallb and configure a loadbalancing ip pool.


Then, install the operator to the your k8s cluster:  
`kubectl apply -f examples/install.yaml`

after which you'll need to install the CRDs:  
`kubectl create -f crds/`

### Using Generic Devices to pass the gpu to the pods  
In order for wolf to render / stream the images you need to pass it a device that can do it, I've an AMD gpu, so I opted to use generic devices to pass the `dri` device

first check the content of `/dev/dri`:  
```
$ ls /dev/dri/
>by-path  card1  renderD128
```
then go to the `examples/generic-devices.yaml` to edit the dri device path to be the same as the desired card.  
```
- path: /dev/dri/renderD128 |->|- path: /dev/dri/renderD128
- path: /dev/dri/cardX      |->|- path: /dev/dri/card1
```

finally apply it:  
`kubectl apply -f examples/generic-devices.yaml`
This should add the gpu as a sharable resource that can be used in the next step.

### Creating a user  
as of now, alex is the hardcoded user, don't change the name in the `examples/user.yaml` or `examples/user_generic_device_gpu.yaml`

However you can change the resources and volume mounts for the sidecars:
- wolf: This one requires a gpu for encoding
- wolf-agent
- pulseaudio


To create a user:  
`kubectl apply -f examples/user_generic_device_gpu.yaml`

### Adding an application and pairing with moonlight  
add the app the to cluster using:  
`kubectl apply -f examples/testball.yaml`

Next get the ip of the loadbalancer service to connect with moonlight:  
`kubectl get svc direwolf -n direwolf -o jsonpath='{.status.loadBalancer.ingress[0].ip}'`

open moonlight to pair with the acquired ip  
then open the pairing page (through Authentik), pick the waiting client and enter the PIN
Moonlight shows.

use it to pair and then connect with the app, it'll take a moment to pull the image, so the first pairing might fail.
