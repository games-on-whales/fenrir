---
paths:
  - pkg/moonlight/rest.go
  - pkg/moonlight/rest_test.go
  - pkg/controllers/session.go
  - pkg/controllers/wolfsession_test.go
  - pkg/controllers/session_ports_test.go
---

# Moonlight client IP → Wolf

- The client IP comes only from `r.RemoteAddr` via `remoteIP` (proxy is hostNetwork, no SNAT).
  - Never take it from headers: the Moonlight ports are internet-facing, so headers are spoofable.
- Wolf's stream sockets are IPv4-only (`udp::v4`/`tcp::v4` upstream) and it matches peers by exact
  IP string, so `wolfSessionFor` refuses non-IPv4 rather than handing Wolf a peer it can't reach.
- There is no default client IP: a placeholder makes Wolf stream to the wrong peer silently.
- Wolf does not read `WOLF_STREAM_CLIENT_IP`; don't reintroduce it as a way to pass the IP.
- `gh` defaults to the `upstream` remote (games-on-whales/fenrir); pass `--repo xerktech/fenrir`.
