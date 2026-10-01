---
paths:
  - pkg/controllers/romm*.go
  - pkg/controllers/catalogue.go
  - images/retroarch/**
  - examples/retroarch.yaml
  - cmd/operator/**
---

# RomM ROMs as RetroArch Apps

- RomM is the ROM manager; the operator only reads `GET /api/roms` (never writes to RomM).
  - Auth is a RomM client API token (`roms.read`) as a Bearer header, from `$ROMM_TOKEN`.
  - Redirects are never followed, so the token only goes to `--romm-url`.
- It reuses `Catalogue.Sync` through its own `Catalogue` holding only `RomMBaseApp`.
  - Never give the Library's Catalogue a RomM base, or the RomM one a Steam/Heroic base: Sync
    deletes every App of each store it has a base for that its game list lacks.
  - Everything in `catalogue.md` about base Apps, per-game settings and art applies.
- Fail closed: a page error, a missing `total`, a count that changes mid-read, a duplicate ID
  or a short read syncs nothing (offset paging shifts when RomM rescans).
  - More than `catalogueMaxGames` ROMs fails; `--romm-collection-id` narrows a big library.
  - So do zero ROMs and every ROM `missing_from_fs`: RomM losing the NAS share would otherwise
    delete every App and its per-game settings (an emptied collection keeps its Apps).
- Platforms are keyed by RomM 5's universal slugs (`psx`, `genesis`, `tg16`, `turbografx-cd`;
  RomM's `backend/utils/platform_slugs.py`), not folder names: RomM aliases folder `ps` to `psx`.
- RomM's paths are relative to its library; sessions mount it read-only at `RomMLibraryPath`
  (`/romm/library`, as RomM's own container). `rommLibraryPath` refuses any path leaving it.
  - Filenames are anyone-with-NAS-write's: the ROM path is `shellQuote`d into the launch line.
- A ROM gets an App only if its platform slug is in `retroArchPlatforms` and one file can be
  chosen (m3u > cue > disc image, ties by name; non-`game` file categories are ignored).
- Every core in `retroArchPlatforms` must be in `images/retroarch`'s `ARG CORES`
  (`TestRetroArchImageHasEveryCore`). Cores need no BIOS where possible: sessions have none.
- `images/retroarch` bakes cores, controller profiles (`autoconfig`) and assets from libretro's
  stable archives, sha256-pinned: GoW's startup script downloads them into the session's
  fresh home on every start. Its `startup-app.sh` runs the launch line through a script,
  since sway's `exec` re-splits arguments.
- Tests: `pkg/controllers/romm_test.go` (fake RomM 5 API).
