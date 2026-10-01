package controllers

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/webp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
)

const (
	// catalogueScanInterval is how often the Library is scanned while it runs.
	// Games are only installed or uninstalled from the Library, so its pod is
	// the only place the catalogue can change (plus one last scan as it stops).
	catalogueScanInterval = 2 * time.Minute

	// catalogueIDAnnotation is the store's own ID of the game (Steam appid,
	// Heroic appName); App names are derived from it and may be lossy.
	catalogueIDAnnotation = "direwolf/catalogue-id"
	// catalogueArtAnnotation is the URL appAssetWebP was fetched from, so art
	// is fetched once, not on every scan.
	catalogueArtAnnotation = "direwolf/catalogue-art"

	// LaunchCommandEnv is set on every container of a catalogue App: the
	// shell command line that starts the game. The base App's container runs
	// it once the session is up (e.g. `exec sh -c "$DIREWOLF_LAUNCH_COMMAND"`).
	LaunchCommandEnv = "DIREWOLF_LAUNCH_COMMAND"

	// A Library that reports more games than this is not believed: every one
	// becomes an App CR.
	catalogueMaxGames = 1000
	// Box art is stored in the App CR (etcd's object limit is 1.5MiB).
	catalogueMaxArtBytes = 1 << 20
	// A game whose art could not be fetched is retried this much later.
	catalogueArtRetry = 6 * time.Hour
	// Moonlight's app title limit (App.spec.title MaxLength).
	catalogueMaxTitle = 63
	// Biggest cover accepted, per side: moonlight-proxy decodes it in full
	// on every /appasset request.
	catalogueMaxArtSide = 4096

	// Size caps on what the Library (user-writable) may hand the operator,
	// whose memory limit is 256Mi: an appmanifest is ~1KiB, Heroic's library
	// caches a few MiB. A file over its cap fails the scan, never drops a game.
	catalogueMaxManifestBytes = 64 << 10
	catalogueMaxStoreBytes    = 8 << 20
	// The art fetched in one periodic scan shares this budget, so new games
	// delay the idle check by at most this. The final scan in stop() fetches
	// none: it runs while the Library holds the Steam lock.
	catalogueArtBudget  = 20 * time.Second
	catalogueArtTimeout = 10 * time.Second
	// catalogueScanTimeout ends the scan script in the pod (coreutils
	// timeout), inside libraryExecTimeout.
	catalogueScanTimeout = "25"
	// vdfMaxDepth bounds the parser's recursion; appmanifests nest 3 deep.
	vdfMaxDepth = 16
)

// Heroic's stores under the Library's ~/.config/heroic (the .deb build).
const (
	heroicLegendaryInstalled = libraryHome + "/.config/heroic/legendaryConfig/legendary/installed.json"
	heroicGOGInstalled       = libraryHome + "/.config/heroic/gog_store/installed.json"
	heroicLegendaryLibrary   = libraryHome + "/.config/heroic/store_cache/legendary_library.json"
	heroicGOGLibrary         = libraryHome + "/.config/heroic/store_cache/gog_library.json"
)

// heroicAppName is what a Heroic appName may look like to be launched: it is
// put into a shell command line and a heroic:// URL.
var heroicAppName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// steamTools matches the names of Steam appmanifests that are not games
// (Proton builds, runtimes, redistributables), not games named "Proton ...".
var steamTools = regexp.MustCompile(`^(Proton( [0-9].*| Experimental| Hotfix| Next| EasyAntiCheat Runtime| BattlEye Runtime)?|Steam Linux Runtime( .*)?|Steamworks Common Redistributables|SteamVR)$`)

// steamFullyInstalled is the StateFlags bit Steam sets once a game is
// installed and not mid-download or mid-update-from-scratch.
const steamFullyInstalled = 4

// artHosts are the only hosts box art is fetched from (https, every redirect
// too). The URLs come from files in the Library, which its desktop user can
// write, so anything else would let them point the operator at in-cluster
// services and read the reply back as an App's box art.
// RomM's covers come from its metadata sources (IGDB, SteamGridDB, libretro
// thumbnails, MobyGames).
var artHosts = []string{
	"steamstatic.com", "akamaihd.net", "epicgames.com", "unrealengine.com", "gog-statics.com", "gog.com",
	"igdb.com", "steamgriddb.com", "libretro.com", "mobygames.com",
}

type CatalogueOptions struct {
	// SteamBaseApp and HeroicBaseApp name the Apps whose spec (pod template,
	// Wolf config, volumes, GPU default) catalogue Apps of that launcher are
	// copied from. Empty: that launcher's games are not catalogued, and its
	// existing catalogue Apps are left alone.
	SteamBaseApp  string
	HeroicBaseApp string
	// RomMBaseApp names the App RomM ROM entries are copied from. Only ever
	// set on RomM's own Catalogue: a Catalogue syncs every store it has a
	// base for, so the Library's would delete every RomM App as uninstalled.
	RomMBaseApp string
}

// catalogueGame is one installed game found in the Library.
type catalogueGame struct {
	Store  string // v1alpha1types.CatalogueStore*
	ID     string // Steam appid or Heroic appName
	Title  string
	ArtURL []string // tried in order
	// Launch is the launch command line of a RomM game (core and ROM path);
	// the other stores' are derived from Store and ID.
	Launch string
}

// Catalogue keeps one App per installed game in sync with the Library's Steam
// and Heroic installs. It is driven by the LibraryController (one worker), so
// it is not safe for concurrent use.
type Catalogue struct {
	Apps      v1alpha1client.AppInterface
	Exec      PodExecutor
	GamesPath string
	// FetchArt returns the image at url; fetchArt by default.
	FetchArt func(ctx context.Context, url string) ([]byte, error)
	CatalogueOptions

	now       func() time.Time
	lastScan  time.Time
	artFailed map[string]time.Time
}

func NewCatalogue(apps v1alpha1client.AppInterface, exec PodExecutor, gamesPath string, options CatalogueOptions) *Catalogue {
	client := newArtClient()
	return &Catalogue{
		Apps:             apps,
		Exec:             exec,
		GamesPath:        gamesPath,
		FetchArt:         func(ctx context.Context, u string) ([]byte, error) { return fetchArt(ctx, client, u) },
		CatalogueOptions: options,
		now:              time.Now,
		artFailed:        map[string]time.Time{},
	}
}

// nonPublicPrefixes are routable-looking ranges that are not the public
// internet: carrier-grade NAT (often cluster/VPN space), benchmarking, and
// IPv6 translations that can reach private IPv4 (NAT64, 6to4).
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("::/96"),        // IPv4-compatible (deprecated)
	netip.MustParsePrefix("2001::/32"),    // Teredo
	netip.MustParsePrefix("240.0.0.0/4"),  // reserved
	netip.MustParsePrefix("192.0.0.0/24"), // IETF protocol assignments
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
}

// publicAddressOnly is a net.Dialer Control refusing non-public peers.
func publicAddressOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("box art address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("box art address %q: %w", address, err)
	}
	if ip = ip.Unmap(); !ip.IsGlobalUnicast() || ip.IsPrivate() || slices.ContainsFunc(nonPublicPrefixes, func(p netip.Prefix) bool { return p.Contains(ip) }) {
		return fmt.Errorf("box art host resolves to non-public address %s", ip)
	}
	return nil
}

// newArtClient fetches box art from artHosts only: every redirect is
// re-checked, and it only connects to public addresses, so an allowed name
// resolving to a cluster or metadata address gets nowhere either.
func newArtClient() *http.Client {
	dialer := &net.Dialer{Timeout: catalogueArtTimeout, Control: publicAddressOnly}
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // net/http's own type
	transport.DialContext = dialer.DialContext
	transport.Proxy = nil // the address check must see the real peer
	return &http.Client{Timeout: catalogueArtTimeout, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return checkArtURL(req.URL)
	}}
}

// ScanDue reports whether a periodic scan is due.
func (c *Catalogue) ScanDue() bool {
	return c.now().Sub(c.lastScan) >= catalogueScanInterval
}

// Scan reads the installed games from the running Library pod and syncs the
// catalogue Apps to them, fetching box art for at most artBudget (0: none).
// A scan that can't read or parse every file changes nothing, so a
// half-written store never uninstalls a game.
func (c *Catalogue) Scan(ctx context.Context, pod *corev1.Pod, artBudget time.Duration) error {
	c.lastScan = c.now()
	ctx, cancel := context.WithTimeout(ctx, libraryExecTimeout+artBudget+30*time.Second)
	defer cancel()
	execCtx, execCancel := context.WithTimeout(ctx, libraryExecTimeout)
	out, err := c.Exec(execCtx, pod.Namespace, pod.Name, libraryContainer, catalogueScanCommand(c.GamesPath))
	execCancel()
	if err != nil {
		return fmt.Errorf("reading the Library's installs: %w", err)
	}
	files, err := readTar(out)
	if err != nil {
		return err
	}
	games, err := parseInstalls(files)
	if err != nil {
		return err
	}
	return c.Sync(ctx, games, artBudget)
}

// catalogueScanCommand tars every appmanifest in the default Steam library
// and in library folders on the games volume (as steamDownloadsCommand looks
// for downloads), plus Heroic's installed-games and library stores. tar frames
// each file with its size, and fails (exit 1) on a file that changed while
// read, so a torn read fails the scan rather than parsing as fewer games.
// Symlinks are followed (-h; tar would otherwise archive an empty entry); a
// dangling one, an unreadable one, or a file over its size cap fails the scan.
func catalogueScanCommand(gamesPath string) []string {
	// As the desktop user (exec runs as root): -h follows symlinks that user
	// planted, which must not reach files only root can read.
	// A path the user can't reach must fail the scan, not read as missing (no
	// games, Apps deleted): reachable checks the deepest existing directory
	// on the way to it can be read and searched. It covers the stores, the
	// default libraries and every library in Steam's libraryfolders.vdf.
	// timeout: cancelling an exec does not kill its process in the pod, so a
	// stuck scan must end itself before libraryExecTimeout.
	return []string{"s6-setuidgid", libraryUnixUser, "timeout", catalogueScanTimeout, "sh", "-c", `
reachable() {
  p=$1
  case $p in /*) ;; *) echo "reachable: $p is not absolute" >&2; exit 1 ;; esac
  while [ ! -e "$p" ] && [ ! -L "$p" ] && [ "$p" != / ]; do
    p=${p%/*}; [ -n "$p" ] || p=/
  done
  if [ -d "$p" ] && { [ ! -r "$p" ] || [ ! -x "$p" ]; }; then
    echo "$p is not readable by $(id -un)" >&2; exit 1
  fi
}
max_manifest=$1 max_store=$2 home=$3 games=$4; shift 4
for f in "$@" "$home/.local/share/Steam/steamapps/x" "$games/x" "$games/steamapps/x"; do reachable "$f"; done
# Only folders Steam lists as libraries must be readable: an unrelated one
# (ext4's root-only lost+found) is none of the scan's business.
# Steam keeps the list in config/ and copies it to steamapps/; read both, in
# the current format ("path" "<dir>") and the pre-2021 one ("1" "<dir>").
# Only absolute paths: Steam on Linux can't use anything else (e.g. a copied
# Windows "D:\\Games"), so they are not libraries to check.
for lf in "$home/.local/share/Steam/config/libraryfolders.vdf" "$home/.local/share/Steam/steamapps/libraryfolders.vdf"; do
  reachable "$lf" # an unreadable config/ must not hide its list
  if [ ! -f "$lf" ]; then
    [ -L "$lf" ] && { echo "$lf is a symlink to no readable file" >&2; exit 1; }
    continue
  fi
  n=$(wc -c < "$lf") || exit 1
  [ "$n" -gt "$max_store" ] && { echo "$lf is $n bytes, over $max_store" >&2; exit 1; }
  libs=$(sed -n -e 's/^[[:space:]]*"path"[[:space:]]*"\(\/.*\)"[[:space:]]*$/\1/p' \
    -e 's/^[[:space:]]*"[0-9][0-9]*"[[:space:]]*"\(\/.*\)"[[:space:]]*$/\1/p' "$lf") || exit 1
  set -f; IFS='
'
  for lib in $libs; do reachable "$lib/steamapps/x"; done
  set +f; unset IFS
done
set -- "$@" "$home"/.local/share/Steam/steamapps/appmanifest_*.acf "$games"/steamapps/appmanifest_*.acf "$games"/*/steamapps/appmanifest_*.acf
for f do
  shift
  if [ ! -f "$f" ]; then
    [ -L "$f" ] && { echo "$f is a dangling symlink" >&2; exit 1; }
    continue
  fi
  case $f in *.acf) max=$max_manifest ;; *) max=$max_store ;; esac
  n=$(wc -c < "$f") || exit 1
  [ "$n" -gt "$max" ] && { echo "$f is $n bytes, over $max" >&2; exit 1; }
  set -- "$@" "$f"
done
[ $# -eq 0 ] && exit 0
exec tar -chf - -- "$@"
`, "sh", strconv.Itoa(catalogueMaxManifestBytes), strconv.Itoa(catalogueMaxStoreBytes), libraryHome, gamesPath,
		heroicLegendaryInstalled, heroicGOGInstalled, heroicLegendaryLibrary, heroicGOGLibrary}
}

// readTar returns the archive's files by name (tar drops the leading /).
// Only regular files within the store size cap are accepted.
func readTar(data string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if data == "" {
		return files, nil
	}
	tr := tar.NewReader(strings.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the Library's installs: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size > catalogueMaxStoreBytes {
			return nil, fmt.Errorf("%s is not a regular file within %d bytes", hdr.Name, catalogueMaxStoreBytes)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", hdr.Name, err)
		}
		files["/"+strings.TrimPrefix(hdr.Name, "/")] = body
	}
}

// parseInstalls turns the scanned files into the installed games. Any file
// that does not parse fails the whole scan.
func parseInstalls(files map[string][]byte) ([]catalogueGame, error) {
	var games []catalogueGame
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasPrefix(path.Base(name), "appmanifest_") {
			continue
		}
		game, ok, err := parseAppManifest(files[name])
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		if ok {
			games = append(games, game)
		}
	}

	heroic, err := parseHeroic(files)
	if err != nil {
		return nil, err
	}
	games = append(games, heroic...)
	if len(games) > catalogueMaxGames {
		return nil, fmt.Errorf("the Library reports %d games, more than the %d catalogued", len(games), catalogueMaxGames)
	}
	return games, nil
}

// parseAppManifest reads a Steam appmanifest_<id>.acf. ok is false for one
// that is not a fully installed game.
func parseAppManifest(data []byte) (game catalogueGame, ok bool, err error) {
	root, err := parseVDF(data)
	if err != nil {
		return game, false, err
	}
	state, _ := root["appstate"].(map[string]any)
	if state == nil {
		return game, false, errors.New("no AppState")
	}
	id, _ := state["appid"].(string)
	// Canonical only: "0570" would be a second App for game 570.
	if n, convErr := strconv.ParseUint(id, 10, 32); convErr != nil || n == 0 || strconv.FormatUint(n, 10) != id {
		return game, false, errors.New("appid is not a canonical non-zero number")
	}
	flags, _ := strconv.ParseUint(fmt.Sprint(state["stateflags"]), 10, 32)
	name, _ := state["name"].(string)
	if flags&steamFullyInstalled == 0 || name == "" {
		return game, false, nil
	}
	if steamTools.MatchString(name) {
		return game, false, nil
	}
	return catalogueGame{
		Store: v1alpha1types.CatalogueStoreSteam,
		ID:    id,
		Title: name,
		ArtURL: []string{
			"https://cdn.cloudflare.steamstatic.com/steam/apps/" + id + "/library_600x900.jpg",
			"https://cdn.cloudflare.steamstatic.com/steam/apps/" + id + "/header.jpg",
		},
	}, true, nil
}

// parseVDF parses Valve's KeyValues text format (what .acf files are) into
// nested maps, keys lowercased (Steam's own lookups are case-insensitive).
func parseVDF(data []byte) (map[string]any, error) {
	p := vdfParser{data: data}
	root, err := p.object(0)
	if err != nil {
		return nil, fmt.Errorf("vdf: %w at byte %d", err, p.pos)
	}
	return root, nil
}

type vdfParser struct {
	data []byte
	pos  int
}

// object parses keys and values up to the closing brace (depth > 0) or the
// end of input (depth 0).
func (p *vdfParser) object(depth int) (map[string]any, error) {
	if depth > vdfMaxDepth {
		return nil, errors.New("nested too deep")
	}
	nested := depth > 0
	obj := map[string]any{}
	for {
		tok, quoted, err := p.token()
		if err != nil {
			return nil, err
		}
		switch {
		case tok == "" && !quoted:
			if nested {
				return nil, errors.New("unexpected end of file")
			}
			return obj, nil
		case tok == "}" && !quoted:
			if !nested {
				return nil, errors.New("unexpected }")
			}
			return obj, nil
		case tok == "{" && !quoted:
			return nil, errors.New("unexpected {")
		}
		key := strings.ToLower(tok)
		val, valQuoted, err := p.token()
		if err != nil {
			return nil, err
		}
		switch {
		case val == "{" && !valQuoted:
			child, err := p.object(depth + 1)
			if err != nil {
				return nil, err
			}
			obj[key] = child
		case (val == "" || val == "}") && !valQuoted:
			// Never quote the file: errors are logged, and the file may be
			// a symlink to something the operator's log must not show.
			return nil, errors.New("key without a value")
		default:
			obj[key] = val
		}
	}
}

// token returns the next token: a quoted or bare string, "{" or "}" (quoted
// false), or "" at the end of input.
func (p *vdfParser) token() (tok string, quoted bool, err error) {
	for p.pos < len(p.data) {
		switch ch := p.data[p.pos]; {
		case ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n':
			p.pos++
		case ch == '/' && p.pos+1 < len(p.data) && p.data[p.pos+1] == '/':
			for p.pos < len(p.data) && p.data[p.pos] != '\n' {
				p.pos++
			}
		case ch == '{' || ch == '}':
			p.pos++
			return string(ch), false, nil
		case ch == '"':
			p.pos++
			var sb strings.Builder
			for p.pos < len(p.data) {
				c := p.data[p.pos]
				p.pos++
				switch {
				case c == '"':
					return sb.String(), true, nil
				case c == '\\' && p.pos < len(p.data):
					next := p.data[p.pos]
					p.pos++
					switch next {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					default:
						sb.WriteByte(next)
					}
				default:
					sb.WriteByte(c)
				}
			}
			return "", false, errors.New("unterminated string")
		default:
			start := p.pos
			for p.pos < len(p.data) && !strings.ContainsRune(" \t\r\n{}\"", rune(p.data[p.pos])) {
				p.pos++
			}
			return string(p.data[start:p.pos]), true, nil
		}
	}
	return "", false, nil
}

// heroicLibraryGame is the part of Heroic's cached GameInfo the catalogue
// uses: the title (GOG's installed store has none) and the portrait cover.
type heroicLibraryGame struct {
	AppName   string `json:"app_name"`
	Title     string `json:"title"`
	ArtSquare string `json:"art_square"`
	ArtCover  string `json:"art_cover"`
}

// parseHeroic reads Heroic's Epic (legendary) and GOG installed-games stores,
// titled and illustrated from its library caches. A missing store means no
// games from it; one that does not parse fails the scan.
func parseHeroic(files map[string][]byte) ([]catalogueGame, error) {
	var epicInstalled map[string]struct {
		AppName string `json:"app_name"`
		Title   string `json:"title"`
		IsDLC   bool   `json:"is_dlc"`
	}
	var gogInstalled struct {
		Installed []struct {
			AppName string `json:"appName"`
			IsDLC   bool   `json:"is_dlc"`
		} `json:"installed"`
	}
	var epicLibrary struct {
		Library []heroicLibraryGame `json:"library"`
	}
	var gogLibrary struct {
		Games []heroicLibraryGame `json:"games"`
	}
	for name, into := range map[string]any{
		heroicLegendaryInstalled: &epicInstalled,
		heroicGOGInstalled:       &gogInstalled,
		heroicLegendaryLibrary:   &epicLibrary,
		heroicGOGLibrary:         &gogLibrary,
	} {
		data, ok := heroicFile(files, name)
		if !ok {
			continue // not there: no games from that store
		}
		// Present but empty is not "no games": electron-store writes
		// atomically, so it is a torn or truncated file.
		if err := json.Unmarshal(data, into); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
	}

	byName := func(lib []heroicLibraryGame) map[string]heroicLibraryGame {
		m := make(map[string]heroicLibraryGame, len(lib))
		for _, g := range lib {
			m[g.AppName] = g
		}
		return m
	}
	game := func(store, appName, title string, lib map[string]heroicLibraryGame) (catalogueGame, bool) {
		if !heroicAppName.MatchString(appName) {
			klog.Warningf("Catalogue: skipping %s game with unusable appName %q", store, appName)
			return catalogueGame{}, false
		}
		info := lib[appName]
		g := catalogueGame{Store: store, ID: appName, Title: firstNonEmpty(title, info.Title, appName)}
		for _, u := range []string{info.ArtSquare, info.ArtCover} {
			if u != "" {
				g.ArtURL = append(g.ArtURL, u)
			}
		}
		return g, true
	}

	var games []catalogueGame
	epicLib := byName(epicLibrary.Library)
	for key, g := range epicInstalled {
		if g.IsDLC {
			continue
		}
		if c, ok := game(v1alpha1types.CatalogueStoreEpic, firstNonEmpty(g.AppName, key), g.Title, epicLib); ok {
			games = append(games, c)
		}
	}
	gogLib := byName(gogLibrary.Games)
	for _, g := range gogInstalled.Installed {
		if g.IsDLC {
			continue
		}
		if c, ok := game(v1alpha1types.CatalogueStoreGOG, g.AppName, "", gogLib); ok {
			games = append(games, c)
		}
	}
	sort.Slice(games, func(i, j int) bool { return games[i].Store+games[i].ID < games[j].Store+games[j].ID })
	return games, nil
}

// heroicFile finds a Heroic store among the scanned files by its path below
// the home directory (tar keeps the path relative to /).
func heroicFile(files map[string][]byte, store string) ([]byte, bool) {
	rel := strings.TrimPrefix(store, libraryHome)
	for name, data := range files {
		if strings.HasSuffix(name, rel) {
			return data, true
		}
	}
	return nil, false
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// catalogueAppName is the App's name for a game: <store>-<id>, with a hash
// of the real ID appended when the ID had to be changed to be a valid name
// (Heroic appNames are case-sensitive and may hold '.' or '_').
func catalogueAppName(store, id string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	safe := strings.Trim(sb.String(), "-")
	if safe == id && len(store)+1+len(safe) <= 63 {
		return store + "-" + safe
	}
	h := fnv.New32a()
	h.Write([]byte(id))
	safe = strings.TrimRight(safe[:min(len(safe), 63-len(store)-10)], "-")
	return fmt.Sprintf("%s-%s-%08x", store, safe, h.Sum32())
}

// catalogueMoonlightID is the game's Moonlight app ID: stable, and in
// 2^30..2^31-1, clear of the small IDs hand-written Apps use.
func catalogueMoonlightID(store, id string) int {
	h := fnv.New32a()
	h.Write([]byte(store + ":" + id))
	return int(h.Sum32()&(1<<30-1) | 1<<30)
}

// launchCommand is the shell command line that starts the game. IDs are
// checked (numeric, heroicAppName) before they get here.
func launchCommand(game catalogueGame) string {
	switch game.Store {
	case v1alpha1types.CatalogueStoreRomM:
		return game.Launch
	case v1alpha1types.CatalogueStoreSteam:
		return "steam -applaunch " + game.ID
	}
	runner := "legendary"
	if game.Store == v1alpha1types.CatalogueStoreGOG {
		runner = "gog"
	}
	return "heroic 'heroic://launch?appName=" + url.QueryEscape(game.ID) + "&runner=" + runner + "'"
}

func truncateTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.TrimSpace(s)
	for utf8.RuneCountInString(s) > catalogueMaxTitle {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// Sync makes the catalogue Apps match games: creates the missing, updates
// the changed and deletes those whose game is gone, for each launcher whose
// base App is set and readable. Box art is fetched for at most artBudget.
func (c *Catalogue) Sync(ctx context.Context, games []catalogueGame, artBudget time.Duration) error {
	bases := map[string]*v1alpha1types.App{}
	var errs []error
	for _, b := range []struct {
		name   string
		stores []string
	}{
		{c.SteamBaseApp, []string{v1alpha1types.CatalogueStoreSteam}},
		{c.HeroicBaseApp, []string{v1alpha1types.CatalogueStoreEpic, v1alpha1types.CatalogueStoreGOG}},
		{c.RomMBaseApp, []string{v1alpha1types.CatalogueStoreRomM}},
	} {
		if b.name == "" {
			continue
		}
		base, err := c.Apps.Get(ctx, b.name, metav1.GetOptions{})
		if err == nil && (base.Spec.Template == nil || len(base.Spec.Template.Spec.Containers) == 0) {
			err = errors.New("it has no pod template containers to launch the game in")
		}
		if err != nil {
			// Its launcher's Apps are left as they are, not deleted.
			errs = append(errs, fmt.Errorf("base App %s: %w", b.name, err))
			continue
		}
		for _, s := range b.stores {
			bases[s] = base
		}
	}
	if len(bases) == 0 {
		return errors.Join(errs...)
	}

	stores := make([]string, 0, len(bases))
	for s := range bases {
		stores = append(stores, s)
	}
	req, err := labels.NewRequirement(v1alpha1types.CatalogueLabel, selection.In, stores)
	if err != nil {
		return fmt.Errorf("building catalogue selector: %w", err)
	}
	// From the API server, not a cache: a stale list would recreate or keep
	// Apps the last scan already handled.
	list, err := c.Apps.List(ctx, metav1.ListOptions{LabelSelector: labels.NewSelector().Add(*req).String()})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("listing catalogue Apps: %w", err))...)
	}
	existing := make(map[string]*v1alpha1types.App, len(list.Items))
	for i := range list.Items {
		existing[list.Items[i].Name] = &list.Items[i]
	}

	artCtx, cancel := context.WithTimeout(ctx, artBudget)
	defer cancel()
	wanted := map[string]bool{}
	ids := map[int]string{}
	for _, game := range games {
		base := bases[game.Store]
		if base == nil {
			continue
		}
		name := catalogueAppName(game.Store, game.ID)
		if wanted[name] {
			continue // the same game in two Steam library folders
		}
		wanted[name] = true
		// Moonlight launches by ID; two games on one ID would launch one.
		id := catalogueMoonlightID(game.Store, game.ID)
		if other, dup := ids[id]; dup {
			errs = append(errs, fmt.Errorf("%s and %s hash to Moonlight ID %d; not cataloguing %s", other, name, id, name))
			continue
		}
		ids[id] = name
		if err := c.syncApp(ctx, artCtx, base, game, name, existing[name]); err != nil {
			errs = append(errs, err)
		}
	}
	for name, app := range existing {
		if wanted[name] {
			continue
		}
		klog.Infof("Catalogue: %s (%s) is no longer installed, deleting its App", app.Spec.Title, name)
		err := c.Apps.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &app.UID}})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			errs = append(errs, fmt.Errorf("deleting App %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// syncApp creates or updates one game's App from its launcher's base App.
// The per-game settings (hidden, gpu) are the user's once the App exists;
// everything else follows the base App and the scan. Box art is the
// scanner's: the fetched cover, or the base App's until one is fetched.
func (c *Catalogue) syncApp(ctx, artCtx context.Context, base *v1alpha1types.App, game catalogueGame, name string, existing *v1alpha1types.App) error {
	if existing == nil {
		// A hand-made App may hold the name; never overwrite it (and don't
		// fetch art for an App that can't be created).
		_, err := c.Apps.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			return fmt.Errorf("an App named %s exists but is not a catalogue App; not cataloguing %s over it", name, game.Title)
		case !apierrors.IsNotFound(err):
			return fmt.Errorf("getting App %s: %w", name, err)
		}
	}

	spec := base.Spec.DeepCopy()
	spec.Title = firstNonEmpty(truncateTitle(game.Title), truncateTitle(game.ID))
	spec.ID = catalogueMoonlightID(game.Store, game.ID)
	spec.Hidden = false
	for i := range spec.Template.Spec.Containers {
		ctr := &spec.Template.Spec.Containers[i]
		ctr.Env = setEnv(ctr.Env, LaunchCommandEnv, launchCommand(game))
	}

	app := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if existing != nil {
		app = existing.DeepCopy()
		spec.Hidden = existing.Spec.Hidden
		spec.GPU = existing.Spec.GPU
	}
	if app.Labels == nil {
		app.Labels = map[string]string{}
	}
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	app.Labels[v1alpha1types.CatalogueLabel] = game.Store
	app.Annotations[catalogueIDAnnotation] = game.ID
	// Keep fetched art; the base App's stands in until there is some (the
	// CRD rejects an empty appAssetWebP).
	if existing != nil && app.Annotations[catalogueArtAnnotation] != "" && len(existing.Spec.AppAssetWebP) > 0 {
		spec.AppAssetWebP = existing.Spec.AppAssetWebP
	}
	if art, from := c.art(artCtx, game, app.Annotations[catalogueArtAnnotation]); art != nil {
		spec.AppAssetWebP = art
		app.Annotations[catalogueArtAnnotation] = from
	}

	if existing == nil {
		app.Spec = *spec
		klog.Infof("Catalogue: %s is installed, creating App %s", app.Spec.Title, name)
		if _, err := c.Apps.Create(ctx, app, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating App %s: %w", name, err)
		}
		return nil
	}
	if reflect.DeepEqual(existing.Spec, *spec) && reflect.DeepEqual(existing.ObjectMeta, app.ObjectMeta) {
		return nil
	}
	app.Spec = *spec
	// Optimistic (resourceVersion from the List): a concurrent user edit wins
	// and is merged on the next scan.
	if _, err := c.Apps.Update(ctx, app, metav1.UpdateOptions{}); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
		return fmt.Errorf("updating App %s: %w", name, err)
	}
	return nil
}

// art returns new box art for the game and the URL it came from, or nil to
// keep what the App has: art already fetched from the first URL that works,
// every URL failing (retried after catalogueArtRetry), or ctx (the scan's
// art budget) done.
func (c *Catalogue) art(ctx context.Context, game catalogueGame, fetchedFrom string) (art []byte, from string) {
	for _, u := range game.ArtURL {
		if u == fetchedFrom {
			return nil, ""
		}
		if ctx.Err() != nil {
			return nil, ""
		}
		if parsed, err := url.Parse(u); err != nil || checkArtURL(parsed) != nil {
			continue
		}
		if failed, ok := c.artFailed[u]; ok && c.now().Sub(failed) < catalogueArtRetry {
			continue
		}
		data, err := c.FetchArt(ctx, u)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "" // out of budget, not the URL's fault
			}
			klog.V(2).Infof("Catalogue: no box art for %s from %s: %v", game.Title, u, err)
			c.artFailed[u] = c.now()
			continue
		}
		delete(c.artFailed, u)
		return data, u
	}
	return nil, ""
}

// setEnv sets name=value in env, replacing any earlier value.
func setEnv(env []corev1.EnvVar, name, value string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env)+1)
	for _, e := range env {
		if e.Name != name {
			out = append(out, e)
		}
	}
	return append(out, corev1.EnvVar{Name: name, Value: value})
}

// checkArtURL allows only https URLs on artHosts.
func checkArtURL(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return fmt.Errorf("box art URL %s is not plain https", u.Redacted())
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range artHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("box art host %q is not allowed", host)
}

// fetchArt downloads a box art image: from an allowed host, at most
// catalogueMaxArtBytes, and a PNG, JPEG or WebP (what moonlight-proxy can
// serve as the cover).
func fetchArt(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing box art URL: %w", err)
	}
	if checkErr := checkArtURL(u); checkErr != nil {
		return nil, checkErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building box art request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching box art: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching box art: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, catalogueMaxArtBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading box art: %w", err)
	}
	if len(data) > catalogueMaxArtBytes {
		return nil, fmt.Errorf("box art is over %d bytes", catalogueMaxArtBytes)
	}
	if !isCoverImage(data) {
		return nil, errors.New("box art is not a PNG, JPEG or WebP of at most 4096x4096")
	}
	return data, nil
}

// isCoverImage reports whether data is an image moonlight-proxy can serve,
// of a size it can afford to decode on every request.
func isCoverImage(data []byte) bool {
	for _, decode := range []func(io.Reader) (image.Config, error){png.DecodeConfig, jpeg.DecodeConfig, webp.DecodeConfig} {
		if cfg, err := decode(bytes.NewReader(data)); err == nil {
			return cfg.Width > 0 && cfg.Height > 0 && cfg.Width <= catalogueMaxArtSide && cfg.Height <= catalogueMaxArtSide
		}
	}
	return false
}
