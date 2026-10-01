package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	dwfake "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
)

const rommTestToken = "rmm_secret"

// fakeRomM serves /api/roms as RomM 5 does (offset pages of
// SimpleRomSchema), from roms, checking the client's token.
type fakeRomM struct {
	mu   sync.Mutex
	roms []map[string]any
	// page, if set, rewrites a page before it is served.
	page     func(offset int, body map[string]any)
	requests []*http.Request
}

func (f *fakeRomM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if r.URL.Path != "/api/roms" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+rommTestToken {
		http.Error(w, `{"detail":"Not authenticated"}`, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	end := min(offset+limit, len(f.roms))
	items := []map[string]any{}
	if offset < end {
		items = f.roms[offset:end]
	}
	body := map[string]any{"items": items, "total": len(f.roms), "limit": limit, "offset": offset}
	if f.page != nil {
		f.page(offset, body)
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}

func rommTestROM(id int, platform, name string, files ...string) map[string]any {
	fileObjs := make([]map[string]any, 0, len(files))
	for _, f := range files {
		fileObjs = append(fileObjs, map[string]any{"full_path": f, "category": nil})
	}
	return map[string]any{
		"id": id, "platform_slug": platform, "name": name, "fs_name_no_ext": name,
		"full_path": files[0], "url_cover": nil, "missing_from_fs": false, "files": fileObjs,
		// Some of the fields the sync ignores.
		"summary": "a game", "igdb_metadata": map[string]any{"genres": []string{"Platform"}},
	}
}

func rommBaseApp() *v1alpha1types.App {
	app := baseApp("retroarch", nil)
	app.Spec.Template.Spec.Containers[0].Image = "retroarch:1"
	return app
}

type rommFixture struct {
	dw   *dwfake.Clientset
	romm *RomM
	srv  *fakeRomM
}

func newRomMFixture(t *testing.T, roms []map[string]any, objs ...runtime.Object) *rommFixture {
	t.Helper()
	f := &rommFixture{dw: dwfake.NewSimpleClientset(objs...), srv: &fakeRomM{roms: roms}}
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	var err error
	f.romm, err = NewRomM(f.dw.DirewolfV1alpha1().Apps(libraryTestNS), ts.URL, rommTestToken+"\n", "retroarch", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.romm.Catalogue.FetchArt = func(_ context.Context, u string) ([]byte, error) {
		if strings.Contains(u, "missing") {
			return nil, errors.New("404")
		}
		return []byte("cover:" + u), nil
	}
	return f
}

func (f *rommFixture) apps(t *testing.T) map[string]*v1alpha1types.App {
	t.Helper()
	list, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	apps := map[string]*v1alpha1types.App{}
	for i := range list.Items {
		apps[list.Items[i].Name] = &list.Items[i]
	}
	return apps
}

func launchEnv(app *v1alpha1types.App) string {
	for _, e := range app.Spec.Template.Spec.Containers[0].Env {
		if e.Name == LaunchCommandEnv {
			return e.Value
		}
	}
	return ""
}

func TestRomMSync(t *testing.T) {
	roms := []map[string]any{
		rommTestROM(1, "snes", "Kirby's Dream Land", "roms/snes/Kirby's Dream Land (USA).sfc"),
		// A multi-disc PS1 game: the playlist is launched, not a track.
		rommTestROM(2, "psx", "Final Fantasy VII",
			"roms/ps/FF7/FF7 (Disc 2).cue", "roms/ps/FF7/FF7 (Disc 1).bin", "roms/ps/FF7/FF7.m3u", "roms/ps/FF7/FF7 (Disc 1).cue"),
		// A multi-track game without a playlist: the first cue sheet.
		rommTestROM(3, "segacd", "Sonic CD", "roms/segacd/Sonic CD/Sonic CD (Track 02).bin", "roms/segacd/Sonic CD/Sonic CD.cue"),
		rommTestROM(4, "3do", "No Core", "roms/3do/game.iso"),
		rommTestROM(5, "nes", "Gone", "roms/nes/gone.nes"),
		rommTestROM(6, "nes", "Escape", "../../etc/passwd"),
		rommTestROM(7, "nes", "Absolute", "/etc/passwd"),
		rommTestROM(8, "gba", "", "/romm/library/roms/gba/Advance Wars.gba"),
		// Several files, none of them launchable on its own.
		rommTestROM(9, "psx", "Ambiguous", "roms/ps/x/a.bin", "roms/ps/x/b.bin"),
		rommTestROM(10, "n64", strings.Repeat("Long Title ", 10), "roms/n64/long.z64"),
		// RomM 4's IGDB slug for what RomM 5 calls genesis.
		rommTestROM(11, "genesis-slash-megadrive", "Sonic", "roms/genesis/sonic.md"),
		rommTestROM(12, "genesis", "Sonic 2", "roms/genesis/sonic2.md"),
		// The live library's folder, which RomM 5.3.1 has no alias for.
		rommTestROM(13, "turbografx-16-slash-pc-engine-cd", "Ys", "roms/pce/Ys.cue"),
	}
	roms[0]["url_cover"] = "//images.igdb.com/igdb/image/upload/t_cover_big/kirby.jpg"
	roms[1]["url_cover"] = "https://images.igdb.com/missing.jpg"
	roms[4]["missing_from_fs"] = true
	roms[7]["fs_name_no_ext"] = "Advance Wars (USA)"
	// A manual next to the game is not a second game file.
	roms[7]["files"] = []map[string]any{
		{"full_path": "/romm/library/roms/gba/Advance Wars.gba", "category": nil},
		{"full_path": "roms/gba/Advance Wars.pdf", "category": "manual"},
	}
	// Over two pages.
	for id := 100; id < 100+rommPageSize; id++ {
		roms = append(roms, rommTestROM(id, "gb", "Filler "+strconv.Itoa(id), "roms/gb/"+strconv.Itoa(id)+".gb"))
	}

	gone := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "romm-999", Namespace: libraryTestNS, UID: "u999",
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreRomM},
	}}
	steamGame := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "steam-570", Namespace: libraryTestNS,
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreSteam},
	}}
	f := newRomMFixture(t, roms, rommBaseApp(), gone, steamGame)
	if err := f.romm.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	apps := f.apps(t)
	for _, name := range []string{"romm-999", "romm-4", "romm-5", "romm-6", "romm-7", "romm-9"} {
		if _, ok := apps[name]; ok {
			t.Errorf("%s exists; want it deleted or never created", name)
		}
	}
	if _, ok := apps["steam-570"]; !ok {
		t.Error("the Library catalogue's steam-570 was deleted by the RomM sync")
	}
	if got, want := len(apps), 2+8+rommPageSize; got != want { // base + steam + ROMs
		t.Errorf("%d Apps, want %d", got, want)
	}

	core := func(c string) string { return "retroarch -L " + retroArchCoresPath + "/" + c + "_libretro.so " }
	for name, want := range map[string]struct{ title, launch string }{
		"romm-1":  {"Kirby's Dream Land [SNES]", core("snes9x") + `'/romm/library/roms/snes/Kirby'\''s Dream Land (USA).sfc'`},
		"romm-2":  {"Final Fantasy VII [PS1]", core("pcsx_rearmed") + `'/romm/library/roms/ps/FF7/FF7.m3u'`},
		"romm-3":  {"Sonic CD [Sega CD]", core("genesis_plus_gx") + `'/romm/library/roms/segacd/Sonic CD/Sonic CD.cue'`},
		"romm-8":  {"Advance Wars (USA) [GBA]", core("mgba") + `'/romm/library/roms/gba/Advance Wars.gba'`},
		"romm-11": {"Sonic [Genesis]", core("genesis_plus_gx") + `'/romm/library/roms/genesis/sonic.md'`},
		"romm-12": {"Sonic 2 [Genesis]", core("genesis_plus_gx") + `'/romm/library/roms/genesis/sonic2.md'`},
		"romm-13": {"Ys [PC Engine CD]", core("mednafen_pce_fast") + `'/romm/library/roms/pce/Ys.cue'`},
		"romm-10": {strings.TrimSpace(strings.Repeat("Long Title ", 10)[:catalogueMaxTitle-len(" [N64]")]) + " [N64]", core("mupen64plus_next") + `'/romm/library/roms/n64/long.z64'`},
	} {
		app := apps[name]
		if app == nil {
			t.Errorf("%s missing", name)
			continue
		}
		if app.Spec.Title != want.title || launchEnv(app) != want.launch {
			t.Errorf("%s: title %q launch %q, want %q %q", name, app.Spec.Title, launchEnv(app), want.title, want.launch)
		}
		if app.Labels[v1alpha1types.CatalogueLabel] != "romm" || app.Spec.Hidden || app.Spec.Template.Spec.Containers[0].Image != "retroarch:1" {
			t.Errorf("%s: labels %v hidden %v, not a visible copy of the base App", name, app.Labels, app.Spec.Hidden)
		}
		if app.Spec.ID != catalogueMoonlightID("romm", strings.TrimPrefix(name, "romm-")) {
			t.Errorf("%s: Moonlight ID %d", name, app.Spec.ID)
		}
	}
	if art := string(apps["romm-1"].Spec.AppAssetWebP); art != "cover:https://images.igdb.com/igdb/image/upload/t_cover_big/kirby.jpg" {
		t.Errorf("romm-1 art = %q, want the IGDB cover over https", art)
	}
	if art := string(apps["romm-2"].Spec.AppAssetWebP); art != "base-art" {
		t.Errorf("romm-2 art = %q, want the base App's while its cover fails", art)
	}

	for _, r := range f.srv.requests {
		q := r.URL.Query()
		if q.Get("with_files") != "true" || q.Get("order_by") != "id" || q.Has("collection_id") {
			t.Errorf("request %s", r.URL)
		}
	}
	if len(f.srv.requests) != 2 {
		t.Errorf("%d requests, want 2 pages", len(f.srv.requests))
	}
}

// Any read of RomM that fails or doesn't add up syncs nothing, so it never
// deletes Apps.
func TestRomMScanFailsClosed(t *testing.T) {
	many := make([]map[string]any, 0, catalogueMaxGames+1)
	for id := range catalogueMaxGames + 1 {
		many = append(many, rommTestROM(id, "nes", "x", "roms/nes/x.nes"))
	}
	twoPages := many[:rommPageSize+1]
	for name, tc := range map[string]struct {
		roms  []map[string]any
		token string
		page  func(offset int, body map[string]any)
		want  string // in the error, if set
	}{
		"bad token":     {roms: twoPages, token: "wrong"},
		"too many ROMs": {roms: many},
		"count changes": {roms: twoPages, page: func(offset int, body map[string]any) {
			if offset > 0 {
				body["total"] = rommPageSize + 2
			}
		}},
		"short read": {roms: twoPages, page: func(offset int, body map[string]any) {
			if offset > 0 {
				body["items"] = []map[string]any{}
			}
		}},
		"duplicate ROM": {roms: twoPages, page: func(offset int, body map[string]any) {
			if offset > 0 {
				body["items"] = many[:1]
			}
		}},
		"no ROMs": {roms: []map[string]any{}},
		"all missing": {roms: twoPages, page: func(_ int, body map[string]any) {
			items := []map[string]any{}
			for _, item := range body["items"].([]map[string]any) { //nolint:forcetypeassert // the fake's own type
				item = maps.Clone(item) // shared with the other cases
				item["missing_from_fs"] = true
				items = append(items, item)
			}
			body["items"] = items
		}},
		// An unplayable ROM left over doesn't make the rest uninstalled.
		"all playable missing": {roms: twoPages, page: func(offset int, body map[string]any) {
			items := []map[string]any{}
			for i, item := range body["items"].([]map[string]any) { //nolint:forcetypeassert // the fake's own type
				item = maps.Clone(item)
				item["missing_from_fs"] = true
				if offset == 0 && i == 0 {
					item["missing_from_fs"], item["platform_slug"] = false, "3do"
				}
				items = append(items, item)
			}
			body["items"] = items
		}, want: "missing"},
		"oversize page": {roms: twoPages, page: func(_ int, body map[string]any) {
			body["padding"] = strings.Repeat(" ", rommMaxPageBytes)
		}, want: "bytes"},
		"no total": {roms: twoPages, page: func(_ int, body map[string]any) { body["total"] = nil }},
		"more than counted": {roms: twoPages, page: func(offset int, body map[string]any) {
			if offset > 0 {
				body["items"] = many[rommPageSize : rommPageSize+3]
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			kept := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
				Name: "romm-999", Namespace: libraryTestNS,
				Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreRomM},
			}}
			f := newRomMFixture(t, tc.roms, rommBaseApp(), kept)
			f.srv.page = tc.page
			if tc.token != "" {
				f.romm.Token = tc.token
			}
			err := f.romm.Scan(context.Background())
			if err == nil {
				t.Fatal("Scan succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Scan: %v, want an error about %q", err, tc.want)
			}
			apps := f.apps(t)
			if _, ok := apps["romm-999"]; !ok || len(apps) != 2 {
				t.Errorf("Apps changed on a failed scan: %v", slices.Collect(maps.Keys(apps)))
			}
		})
	}
}

// The token only ever goes to RomM: a redirect is an error, not followed.
func TestRomMDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
	}))
	defer elsewhere.Close()
	redirect := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/api/roms", http.StatusFound))
	defer redirect.Close()
	r, err := NewRomM(dwfake.NewSimpleClientset(rommBaseApp()).DirewolfV1alpha1().Apps(libraryTestNS), redirect.URL, rommTestToken, "retroarch", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Scan(context.Background()); err == nil {
		t.Error("Scan followed a redirect without error")
	}
	if leaked {
		t.Error("the token was sent to the redirect target")
	}
}

func TestRomMCollection(t *testing.T) {
	f := newRomMFixture(t, []map[string]any{rommTestROM(1, "nes", "x", "roms/nes/x.nes")}, rommBaseApp())
	f.romm.CollectionID = 7
	if err := f.romm.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.requests[0].URL.Query().Get("collection_id"); got != "7" {
		t.Errorf("collection_id = %q, want 7", got)
	}
}

func TestNewRomMValidates(t *testing.T) {
	apps := dwfake.NewSimpleClientset().DirewolfV1alpha1().Apps(libraryTestNS)
	for _, tc := range []struct{ url, token, app string }{
		{"ftp://romm", "t", "a"},
		{"http://user:pw@romm", "t", "a"},
		{"http://romm?x=1", "t", "a"},
		{"romm:8080", "t", "a"},
		{"http://romm", "", "a"},
		{"http://romm", "a b", "a"},
		{"http://romm", "t", ""},
	} {
		if _, err := NewRomM(apps, tc.url, tc.token, tc.app, 0); err == nil {
			t.Errorf("NewRomM(%q, %q, %q) accepted", tc.url, tc.token, tc.app)
		}
	}
	if _, err := NewRomM(apps, "http://romm.games.svc:8080/", "t", "a", -1); err == nil {
		t.Error("negative collection accepted")
	}
	r, err := NewRomM(apps, "http://romm.games.svc:8080/sub", "t\n", "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Token != "t" || r.Catalogue.SteamBaseApp != "" || r.Catalogue.HeroicBaseApp != "" {
		t.Errorf("token %q, catalogue %+v: want a trimmed token and only the RomM base", r.Token, r.Catalogue.CatalogueOptions)
	}
}

// The launch command is one shell line: hostile file names stay one word.
func TestRomMLaunchCommandQuoting(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	name := `it's $(touch pwned) "x" ; rm -rf ` + "`id`" + ` \ end.sfc`
	games := rommGames([]rommROM{{ID: 1, PlatformSlug: "snes", Name: "x", Files: []rommFile{{FullPath: "roms/snes/" + name}}}})
	if len(games) != 1 {
		t.Fatalf("games = %+v", games)
	}
	// Run it with retroarch replaced by printing its last argument.
	script := `retroarch() { for a; do last=$a; done; printf %s "$last"; }; ` + games[0].Launch
	out, err := exec.CommandContext(t.Context(), "sh", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := RomMLibraryPath + "/roms/snes/" + name; string(out) != want {
		t.Errorf("ROM path = %q, want %q", out, want)
	}
}

func TestRomMLibraryPath(t *testing.T) {
	for in, want := range map[string]string{
		"roms/nes/a.nes":               "/romm/library/roms/nes/a.nes",
		"/romm/library/roms/nes/a.nes": "/romm/library/roms/nes/a.nes",
		"nes/roms/a b.nes":             "/romm/library/nes/roms/a b.nes",
		"..":                           "",
		"roms/../../x":                 "",
		"/etc/passwd":                  "",
		"/romm/library/../x":           "",
		"roms//nes/a":                  "",
		"roms/nes/a\nb":                "",
		"":                             "",
	} {
		got, err := rommLibraryPath(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("rommLibraryPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// Every core the operator launches is installed by images/retroarch.
func TestRetroArchImageHasEveryCore(t *testing.T) {
	dockerfile, err := os.ReadFile("../../images/retroarch/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG CORES="([^"]*)"$`).FindSubmatch(dockerfile)
	if m == nil {
		t.Fatal("no ARG CORES in images/retroarch/Dockerfile")
	}
	installed := strings.Fields(string(m[1]))
	for slug, p := range retroArchPlatforms {
		if !slices.Contains(installed, p.Core) {
			t.Errorf("platform %s uses core %s, which images/retroarch does not install", slug, p.Core)
		}
	}
	if !strings.Contains(string(dockerfile), retroArchCoresPath+"/") {
		t.Errorf("images/retroarch does not install its cores in %s", retroArchCoresPath)
	}
}

// The RomM Catalogue only touches RomM Apps; the Library's never touches them.
func TestLibraryCatalogueLeavesRomMApps(t *testing.T) {
	rommApp := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "romm-1", Namespace: libraryTestNS,
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreRomM},
	}, Spec: v1alpha1types.AppSpec{Template: &corev1.PodTemplateSpec{}}}
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil), rommBaseApp(), rommApp)
	f.scan(t)
	f.app(t, "romm-1")
}
