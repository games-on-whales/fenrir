---
paths:
  - pkg/moonlight/**
  - cmd/moonlight-proxy/**
  - pkg/controllers/session.go
---

# moonlight-proxy

- An error the Moonlight user should read goes out as HTTP 200 with the error in the XML
  `status_code`/`status_message` (as Sunshine does), via `sendXMLWithHTTPStatus`.
  - moonlight-qt turns a non-2xx HTTP status into a transport error and never shows the message.
- `--launch-timeout` must stay under moonlight-qt's 120s launch request timeout, or the client
  gives up first and shows a generic error while the session keeps starting.
  - The operator's `unstartedSessionTTL` reaper must outlast it, or cold starts get reaped mid-launch.
    Tests: `TestUnstartedSessionTTLOutlastsLaunchTimeout` in `pkg/controllers/reaper_test.go`.
- The session limit (`createSession`) holds only because the check and the Create share
  `launchSlot` and count users from a live `List`, not the informer.
  - An orphaned Session from a failed launch would lock every other user out.
  - It is per process: running more than one moonlight-proxy replica breaks it.
  - Tests: `TestLaunchConcurrentUsersRespectLimit` in `pkg/moonlight/launch_test.go`.
  - Work under the slot is bounded by `launchSlotTimeout` and by the launch's own deadline:
    slot queue + slot work + readiness wait all share one `LaunchTimeout` budget.
  - Sessions carry `direwolf/launch-id`; a failed launch (even a failed Create) deletes by it.
    A failed Create is cleaned up under the slot; a failed wait queues its launch-id, answers,
    and the next slot holder (or a background drainer) deletes it before counting.
  - A drain shares its caller's deadline and requeues what it didn't finish; never detach it
    inside `createSession`, or stalled cleanups eat the next launch's budget.
- `RESTServerOptions.BusyCheck` is the hook for anything besides Sessions that holds the host
  (the Library pod, via `LibraryBusyCheck`). A check error fails the launch closed.
  - It runs again after the Create and backs the Session out if busy (see `library.md`).
