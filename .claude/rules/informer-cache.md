---
paths:
  - pkg/controllers/session*.go
  - pkg/controllers/catalogue*.go
  - pkg/controllers/library*.go
  - pkg/controllers/romm*.go
---

# Objects from informers are the cache's own

- `SessionController` runs 2 workers, so two Sessions (often of the same App/User) reconcile
  at once. Anything read from an informer/lister is shared cache memory.
- Never write into it: `DeepCopy()` structs/ObjectMeta, `maps.Clone` / `slices.Clone` before
  adding to a map or appending to a slice taken from it.
- `append` onto a cached slice is a write too: JSON-decoded slices carry spare capacity
  (3 items decode as len 3, cap 4), so the append lands in the cache's backing array.
- Tests: `TestBuildPodLeavesCachedObjects` (session_test.go) — run it under `-race`.
