package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
)

const (
	// rommScanInterval is how often RomM's ROM list is read. RomM rescans
	// its library itself; the operator only mirrors what RomM reports.
	rommScanInterval = 5 * time.Minute
	rommPageSize     = 100
	// A page of 100 ROMs with their metadata and files is a few hundred KiB;
	// the operator's memory limit is 256Mi.
	rommMaxPageBytes  = 16 << 20
	rommClientTimeout = 30 * time.Second

	// RomMLibraryPath is where the RomM base App must mount RomM's library
	// (the NAS ROM share), read-only: where RomM's own container mounts it,
	// and the root RomM's file paths are relative to.
	RomMLibraryPath = "/romm/library"
	// retroArchCoresPath is where images/retroarch installs its cores.
	retroArchCoresPath = "/usr/lib/x86_64-linux-gnu/libretro"
)

// retroArchPlatform is how a RomM platform is played: its libretro core
// (installed by images/retroarch) and the short name added to App titles,
// since one game is often in RomM for several platforms.
type retroArchPlatform struct {
	Core  string
	Label string
}

// retroArchPlatforms maps RomM platform slugs to their core. RomM 5 reports
// its universal slugs (backend/utils/platform_slugs.py: "psx", not the
// folder name "ps"); the older IGDB slugs it migrated away from are kept for
// RomM 4. ROMs of other platforms get no App. Cores that run without BIOS
// files are preferred: sessions have none.
var retroArchPlatforms = map[string]retroArchPlatform{
	"nes":           {"fceumm", "NES"},
	"famicom":       {"fceumm", "Famicom"},
	"fds":           {"fceumm", "FDS"},
	"snes":          {"snes9x", "SNES"},
	"sfam":          {"snes9x", "Super Famicom"},
	"n64":           {"mupen64plus_next", "N64"},
	"gb":            {"gambatte", "GB"},
	"gbc":           {"gambatte", "GBC"},
	"gba":           {"mgba", "GBA"},
	"nds":           {"melondsds", "DS"},
	"sms":           {"genesis_plus_gx", "Master System"}, //nolint:goconst // a table
	"gamegear":      {"genesis_plus_gx", "Game Gear"},
	"genesis":       {"genesis_plus_gx", "Genesis"},
	"segacd":        {"genesis_plus_gx", "Sega CD"},
	"sega32":        {"picodrive", "32X"},
	"saturn":        {"yabasanshiro", "Saturn"},
	"psx":           {"pcsx_rearmed", "PS1"},
	"atari2600":     {"stella", "Atari 2600"},
	"atari8bit":     {"atari800", "Atari 8-bit"},
	"arcade":        {"fbneo", "Arcade"},
	"tg16":          {"mednafen_pce_fast", "PC Engine"}, //nolint:goconst // a table
	"turbografx-cd": {"mednafen_pce_fast", "PC Engine CD"},

	// RomM 4's IGDB slugs.
	"ps":                               {"pcsx_rearmed", "PS1"},
	"genesis-slash-megadrive":          {"genesis_plus_gx", "Genesis"},
	"turbografx16--1":                  {"mednafen_pce_fast", "PC Engine"},
	"turbografx-16-slash-pc-engine-cd": {"mednafen_pce_fast", "PC Engine CD"},
}

// rommROM is the part of RomM's SimpleRomSchema the sync reads.
type rommROM struct {
	ID            int        `json:"id"`
	PlatformSlug  string     `json:"platform_slug"`
	Name          string     `json:"name"`
	FsNameNoExt   string     `json:"fs_name_no_ext"`
	FullPath      string     `json:"full_path"`
	URLCover      string     `json:"url_cover"`
	MissingFromFS bool       `json:"missing_from_fs"`
	Files         []rommFile `json:"files"`
}

type rommFile struct {
	FullPath string `json:"full_path"`
	// Category is null for the game itself; set for DLC, patches, manuals...
	Category string `json:"category"`
}

type rommPage struct {
	Items []rommROM `json:"items"`
	Total *int      `json:"total"`
}

// RomM keeps one App per playable ROM in RomM in sync with RomM's API: each
// a copy of the base App, launching the ROM's RetroArch core on the ROM.
// RomM stays the ROM manager; nothing is ever written to it.
type RomM struct {
	// URL is RomM's base URL (e.g. http://romm.games.svc:8080).
	URL *url.URL
	// Token is a RomM client API token with roms.read.
	Token string
	// CollectionID, if set, limits the Apps to one RomM collection.
	CollectionID int
	Client       *http.Client
	Catalogue    *Catalogue
}

// NewRomM syncs RomM's ROMs into Apps copied from baseApp.
func NewRomM(apps v1alpha1client.AppInterface, rawURL, token, baseApp string, collectionID int) (*RomM, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing RomM URL: %w", err)
	}
	plain := u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
	if !plain || (u.Scheme != "http" && u.Scheme != "https") { //nolint:goconst // URL schemes
		return nil, errors.New("RomM URL must be a plain http(s)://host[:port][/path] URL")
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsFunc(token, unicode.IsSpace) {
		return nil, errors.New("RomM token must be set and hold no whitespace")
	}
	if baseApp == "" {
		return nil, errors.New("RomM needs a base App")
	}
	if collectionID < 0 {
		return nil, fmt.Errorf("RomM collection ID must not be negative, got %d", collectionID)
	}
	// Only the base App for RomM: a Catalogue syncs (and deletes) the Apps
	// of every store it has a base App for.
	catalogue := NewCatalogue(apps, nil, "", CatalogueOptions{RomMBaseApp: baseApp})
	return &RomM{
		URL:          u,
		Token:        token,
		CollectionID: collectionID,
		// Redirects are not followed: the token must only go to RomM.
		Client: &http.Client{Timeout: rommClientTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		Catalogue: catalogue,
	}, nil
}

// Run syncs every rommScanInterval until ctx is done. A failed sync is
// logged and changes nothing.
func (r *RomM) Run(ctx context.Context) error {
	for {
		if err := r.Scan(ctx); err != nil && ctx.Err() == nil {
			klog.Errorf("RomM: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // the caller checks for context.Canceled
		case <-time.After(rommScanInterval):
		}
	}
}

// Scan reads every ROM from RomM and syncs the Apps to the playable ones. A
// read that fails, or doesn't add up, syncs nothing: a short list would
// delete Apps (and their per-game settings).
func (r *RomM) Scan(ctx context.Context) error {
	roms, err := r.list(ctx)
	if err != nil {
		return err
	}
	return r.Catalogue.Sync(ctx, rommGames(roms), catalogueArtBudget)
}

// list pages through RomM's ROMs in ID order, failing if RomM's count
// changes along the way (a rescan shifts the offsets).
func (r *RomM) list(ctx context.Context) ([]rommROM, error) {
	var roms []rommROM
	total := -1
	for {
		page, err := r.page(ctx, len(roms))
		if err != nil {
			return nil, err
		}
		if page.Total == nil || *page.Total < 0 {
			return nil, errors.New("RomM's ROM list has no total")
		}
		switch {
		case total == -1:
			total = *page.Total
			if total > catalogueMaxGames {
				return nil, fmt.Errorf("RomM reports %d ROMs, more than the %d catalogued (narrow it with a collection)", total, catalogueMaxGames)
			}
		case *page.Total != total:
			return nil, errors.New("RomM's ROM count changed while it was read")
		}
		roms = append(roms, page.Items...)
		if len(roms) > total {
			return nil, errors.New("RomM returned more ROMs than it counted")
		}
		if len(roms) == total {
			break
		}
		if len(page.Items) == 0 {
			return nil, fmt.Errorf("RomM returned %d of the %d ROMs it counted", len(roms), total)
		}
	}
	// RomM losing the share (an NFS outage during a rescan) would read as
	// every ROM uninstalled.
	if total == 0 {
		return nil, errors.New("RomM reports no ROMs")
	}
	if !slices.ContainsFunc(roms, func(r rommROM) bool {
		_, playable := retroArchPlatforms[r.PlatformSlug]
		return playable && !r.MissingFromFS
	}) {
		return nil, errors.New("RomM reports every ROM of a RetroArch platform missing from its library")
	}
	seen := make(map[int]bool, len(roms))
	for _, rom := range roms {
		if seen[rom.ID] {
			return nil, errors.New("RomM returned a ROM twice (its list changed while it was read)")
		}
		seen[rom.ID] = true
	}
	return roms, nil
}

func (r *RomM) page(ctx context.Context, offset int) (*rommPage, error) {
	q := url.Values{
		"limit":      {strconv.Itoa(rommPageSize)},
		"offset":     {strconv.Itoa(offset)},
		"order_by":   {"id"},
		"order_dir":  {"asc"},
		"with_files": {"true"},
		"with_total": {"true"},
		// Indexes the RomM UI uses; costly to build, and unused here.
		"with_char_index":    {"false"},
		"with_filter_values": {"false"},
		"with_rom_id_index":  {"false"},
	}
	if r.CollectionID != 0 {
		q.Set("collection_id", strconv.Itoa(r.CollectionID))
	}
	u := r.URL.JoinPath("api", "roms")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building RomM request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing RomM's ROMs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing RomM's ROMs: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, rommMaxPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading RomM's ROM list: %w", err)
	}
	if len(data) > rommMaxPageBytes {
		return nil, fmt.Errorf("a page of RomM's ROM list is over %d bytes", rommMaxPageBytes)
	}
	var page rommPage
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("parsing RomM's ROM list: %w", err)
	}
	return &page, nil
}

// rommGames is the playable ROMs as catalogue games. A ROM is skipped (its
// App, if any, deleted) when RomM has lost its file, its platform has no
// core, or no single file to launch can be told apart.
func rommGames(roms []rommROM) []catalogueGame {
	games := make([]catalogueGame, 0, len(roms))
	for i := range roms {
		rom := &roms[i]
		platform, ok := retroArchPlatforms[rom.PlatformSlug]
		if !ok {
			klog.V(2).Infof("RomM: not cataloguing ROM %d: no RetroArch core for platform %q", rom.ID, rom.PlatformSlug)
			continue
		}
		if rom.MissingFromFS {
			continue
		}
		file, err := rommLaunchFile(rom)
		if err != nil {
			klog.V(2).Infof("RomM: not cataloguing ROM %d: %v", rom.ID, err)
			continue
		}
		game := catalogueGame{
			Store:  v1alpha1types.CatalogueStoreRomM,
			ID:     strconv.Itoa(rom.ID),
			Title:  rommTitle(rom, platform),
			Launch: "retroarch -L " + path.Join(retroArchCoresPath, platform.Core+"_libretro.so") + " " + shellQuote(file),
		}
		if cover := rom.URLCover; cover != "" {
			if strings.HasPrefix(cover, "//") { // IGDB's protocol-relative URLs
				cover = "https:" + cover
			}
			game.ArtURL = []string{cover}
		}
		games = append(games, game)
	}
	return games
}

// rommTitle is the ROM's name with its platform's label, within Moonlight's
// title limit.
func rommTitle(rom *rommROM, platform retroArchPlatform) string {
	suffix := " [" + platform.Label + "]"
	name := []rune(firstNonEmpty(truncateTitle(rom.Name), truncateTitle(rom.FsNameNoExt)))
	if n := catalogueMaxTitle - utf8.RuneCountInString(suffix); len(name) > n {
		name = name[:n]
	}
	return strings.TrimSpace(string(name)) + suffix
}

// rommLaunchExtRank orders the files of a multi-file ROM: a playlist, then
// a cue sheet, then a disc image is what RetroArch is pointed at.
var rommLaunchExtRank = map[string]int{".m3u": 0, ".cue": 1, ".gdi": 2, ".ccd": 2, ".chd": 2, ".iso": 2, ".pbp": 2}

// rommLaunchFile is the absolute path, under RomMLibraryPath, of the file
// RetroArch loads for the ROM: its only game file, or the best-ranked one of
// several (the first by name, i.e. disc 1, on a tie).
func rommLaunchFile(rom *rommROM) (string, error) {
	var files []string
	for _, f := range rom.Files {
		if f.Category == "" || f.Category == "game" {
			files = append(files, f.FullPath)
		}
	}
	if len(rom.Files) == 0 {
		files = []string{rom.FullPath}
	}
	if len(files) == 0 {
		return "", errors.New("it has no game file")
	}
	if len(files) > 1 {
		rank := func(f string) int {
			if r, ok := rommLaunchExtRank[strings.ToLower(path.Ext(f))]; ok {
				return r
			}
			return len(rommLaunchExtRank)
		}
		slices.SortFunc(files, func(a, b string) int {
			if d := rank(a) - rank(b); d != 0 {
				return d
			}
			return strings.Compare(a, b)
		})
		if rank(files[0]) == len(rommLaunchExtRank) {
			return "", fmt.Errorf("%d files and none of them a playlist, cue sheet or disc image", len(files))
		}
	}
	return rommLibraryPath(files[0])
}

// rommLibraryPath turns a path RomM reports (relative to its library) into
// one under RomMLibraryPath, refusing any that would leave it.
func rommLibraryPath(p string) (string, error) {
	p = strings.TrimPrefix(p, RomMLibraryPath+"/")
	if p == "" || !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl) {
		return "", errors.New("its file path is empty or not printable")
	}
	if path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return "", errors.New("its file path is not a clean path inside RomM's library")
	}
	return path.Join(RomMLibraryPath, p), nil
}

// shellQuote quotes s as one shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
