package controllers

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/remotecommand"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	dwfake "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
)

// fixtureExec runs the Library's exec commands on this machine, with the
// pod's /config (home) and /games mounts mapped onto testdata/catalogue.
func fixtureExec(t *testing.T, dir string) PodExecutor {
	t.Helper()
	dir, err := filepath.Abs(dir) // the pod's paths are absolute
	if err != nil {
		t.Fatal(err)
	}
	home, games := filepath.Join(dir, "home"), filepath.Join(dir, "games")
	return func(ctx context.Context, _, pod, container string, command []string) (string, error) {
		if pod != LibraryPodName || container != libraryContainer {
			return "", errors.New("exec into the wrong container")
		}
		// The pod runs it as the desktop user; here, as whoever runs the test.
		if len(command) < 2 || command[0] != "s6-setuidgid" || command[1] != "abc" { // the image's desktop user
			return "", errors.New("scan not run as the desktop user: " + strings.Join(command, " "))
		}
		args := slices.Clone(command[2:])
		if len(args) < 2 || args[0] != "timeout" {
			return "", errors.New("scan not bounded by timeout: " + strings.Join(command, " "))
		}
		for i, a := range args {
			switch {
			case a == libraryHome || strings.HasPrefix(a, libraryHome+"/"):
				args[i] = home + strings.TrimPrefix(a, libraryHome)
			case a == "/games":
				args[i] = games
			}
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // the Library's own scan command, on fixtures
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return stdout.String(), errors.New(err.Error() + ": " + stderr.String())
		}
		return stdout.String(), nil
	}
}

func scanFixture(t *testing.T, dir string) []catalogueGame {
	t.Helper()
	out, err := fixtureExec(t, dir)(context.Background(), libraryTestNS, LibraryPodName, libraryContainer, catalogueScanCommand("/games"))
	if err != nil {
		t.Fatalf("scan command: %v", err)
	}
	files, err := readTar(out)
	if err != nil {
		t.Fatal(err)
	}
	games, err := parseInstalls(files)
	if err != nil {
		t.Fatal(err)
	}
	return games
}

// The real scan script over fixture Steam libraries and Heroic stores: only
// fully installed games, no Steam tools, no DLC, no appName that isn't safe
// to put in a launch command.
func TestCatalogueScanFixtures(t *testing.T) {
	games := scanFixture(t, "testdata/catalogue")

	got := map[string]catalogueGame{}
	for _, g := range games {
		got[g.Store+"/"+g.ID] = g
	}
	want := map[string]string{
		"steam/570":      "Dota 2",
		"steam/1245620":  `ELDEN RING "Shadow" Edition`, // StateFlags 6: installed, update pending
		"epic/Quail":     "Hades",
		"gog/1207658924": "Unreal Tournament 2004",
	}
	if len(got) != len(want) {
		t.Errorf("scanned %d games, want %d: %+v", len(got), len(want), games)
	}
	for key, title := range want {
		if got[key].Title != title {
			t.Errorf("%s: title %q, want %q", key, got[key].Title, title)
		}
	}
	if art := got["epic/Quail"].ArtURL; len(art) == 0 || art[0] != "https://cdn1.epicgames.com/quail/hades-tall.jpg" {
		t.Errorf("Hades art = %v, want the tall cover first", art)
	}
	if art := got["steam/570"].ArtURL; len(art) == 0 || !strings.HasSuffix(art[0], "/570/library_600x900.jpg") {
		t.Errorf("Dota 2 art = %v, want the 600x900 library cover first", art)
	}
}

// An empty Library (no Steam, no Heroic yet) is no games, not an error.
func TestCatalogueScanEmptyLibrary(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"home", "games"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if games := scanFixture(t, dir); len(games) != 0 {
		t.Errorf("games = %+v, want none", games)
	}
}

// A store that doesn't parse (e.g. read mid-write) fails the scan, so the
// sync never sees it as every game uninstalled.
func TestCatalogueScanCorruptStoreFails(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"/config/.config/heroic/gog_store/installed.json", `{"installed": [`},
		{"/games/steamapps/appmanifest_1.acf", `"AppState" { "appid" "1" `},
		{"/games/steamapps/appmanifest_2.acf", `"AppState" { "appid" "abc" "name" "x" "StateFlags" "4" }`},
	} {
		if _, err := parseInstalls(map[string][]byte{tc.name: []byte(tc.data)}); err == nil {
			t.Errorf("%s: parsed %q without error", tc.name, tc.data)
		}
	}
}

func TestParseVDF(t *testing.T) {
	got, err := parseVDF([]byte("// c\n\"A\" { \"K\" \"v \\\"q\\\" \\\\\" Bare { \"x\" \"1\" } }"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": map[string]any{"k": `v "q" \`, "bare": map[string]any{"x": "1"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseVDF = %#v, want %#v", got, want)
	}
	for _, bad := range []string{`"a" {`, `}`, `"a"`, `"a" "b`, `{ }`} {
		if _, err := parseVDF([]byte(bad)); err == nil {
			t.Errorf("parseVDF(%q) succeeded", bad)
		}
	}
}

func TestCatalogueAppName(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range []struct{ store, id, want string }{
		{"steam", "570", "steam-570"},
		{"gog", "1207658924", "gog-1207658924"},
		{"epic", "quail", "epic-quail"},
		{"epic", "Quail", ""}, // case changes: hashed, distinct from "quail"
		{"epic", "a.b_c", ""},
		{"epic", strings.Repeat("x", 128), ""},
		{"epic", "...", ""},
	} {
		got := catalogueAppName(tc.store, tc.id)
		if tc.want != "" && got != tc.want {
			t.Errorf("catalogueAppName(%q, %q) = %q, want %q", tc.store, tc.id, got, tc.want)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 || len(got) > 63 {
			t.Errorf("catalogueAppName(%q, %q) = %q is not a valid name: %v", tc.store, tc.id, got, errs)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q both map to %q", prev, tc.id, got)
		}
		seen[got] = tc.id
	}
}

func TestCatalogueMoonlightID(t *testing.T) {
	a, b := catalogueMoonlightID("steam", "570"), catalogueMoonlightID("gog", "570")
	if a == b {
		t.Error("the same ID in two stores got one Moonlight ID")
	}
	for _, id := range []int{a, b} {
		if id < 1<<30 || id > 1<<31-1 {
			t.Errorf("Moonlight ID %d outside 2^30..2^31-1", id)
		}
	}
	if catalogueMoonlightID("steam", "570") != a {
		t.Error("Moonlight ID is not stable")
	}
}

func TestLaunchCommand(t *testing.T) {
	for _, tc := range []struct {
		game catalogueGame
		want string
	}{
		{catalogueGame{Store: v1alpha1types.CatalogueStoreSteam, ID: "570"}, "steam -applaunch 570"},
		{catalogueGame{Store: v1alpha1types.CatalogueStoreEpic, ID: "Quail"}, "heroic 'heroic://launch?appName=Quail&runner=legendary'"},
		{catalogueGame{Store: v1alpha1types.CatalogueStoreGOG, ID: "1207658924"}, "heroic 'heroic://launch?appName=1207658924&runner=gog'"},
	} {
		if got := launchCommand(tc.game); got != tc.want {
			t.Errorf("launchCommand(%+v) = %q, want %q", tc.game, got, tc.want)
		}
	}
}

func TestCheckArtURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://cdn.cloudflare.steamstatic.com/steam/apps/570/library_600x900.jpg": true,
		"https://cdn1.epicgames.com/x.jpg":                                          true,
		"https://images.gog-statics.com/x.jpg":                                      true,
		"https://images.gog-statics.com:443/x.jpg":                                  true,
		"http://cdn.cloudflare.steamstatic.com/x.jpg":                               false,
		"https://steamstatic.com.evil.example/x.jpg":                                false,
		"https://evilsteamstatic.com/x.jpg":                                         false,
		"https://user:pw@cdn1.epicgames.com/x.jpg":                                  false,
		"https://cdn1.epicgames.com:8443/x.jpg":                                     false,
		"https://10.0.0.1/x.jpg":                                                    false,
		"https://kubernetes.default.svc/api":                                        false,
		"fallback":                                                                  false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := checkArtURL(u) == nil; got != ok {
			t.Errorf("checkArtURL(%s) allowed = %v, want %v", raw, got, ok)
		}
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 6, 9))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fetchArt against a local TLS server standing in for every allowed host.
func TestFetchArt(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			_, _ = w.Write(pngData)
		case "/big.png":
			_, _ = w.Write(append(slices.Clone(pngData), make([]byte, catalogueMaxArtBytes)...))
		case "/wide.png":
			var buf bytes.Buffer
			_ = png.Encode(&buf, image.NewGray(image.Rect(0, 0, catalogueMaxArtSide+1, 1)))
			_, _ = w.Write(buf.Bytes())
		case "/html":
			_, _ = w.Write([]byte("<html>not an image</html>"))
		case "/redirect-out":
			// Served (the transport dials this server for every host), so
			// only the client's redirect check refuses it.
			http.Redirect(w, r, "https://169.254.169.254/ok.png", http.StatusFound)
		case "/redirect-in":
			http.Redirect(w, r, "https://cdn1.epicgames.com/ok.png", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("test server client has no *http.Transport")
	}
	transport := base.Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	transport.TLSClientConfig.InsecureSkipVerify = true // the test server stands in for every host
	// The production client (its redirect check), dialling the test server.
	client := newArtClient()
	client.Transport = transport

	for path, wantOK := range map[string]bool{
		"/ok.png":       true,
		"/redirect-in":  true,
		"/big.png":      false,
		"/wide.png":     false,
		"/html":         false,
		"/missing":      false,
		"/redirect-out": false,
	} {
		data, err := fetchArt(context.Background(), client, "https://cdn1.epicgames.com"+path)
		if (err == nil) != wantOK {
			t.Errorf("fetchArt(%s): err = %v, want ok=%v", path, err, wantOK)
		}
		if wantOK && !bytes.Equal(data, pngData) {
			t.Errorf("fetchArt(%s) returned %d bytes, want the PNG", path, len(data))
		}
	}
	if _, err := fetchArt(context.Background(), client, "https://"+srv.Listener.Addr().String()+"/ok.png"); err == nil {
		t.Error("fetchArt fetched from a host that is not allowed")
	}
}

func baseApp(name string, gpu *v1alpha1types.AppGPU) *v1alpha1types.App {
	return &v1alpha1types.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: libraryTestNS},
		Spec: v1alpha1types.AppSpec{
			Title:        name,
			ID:           2,
			Hidden:       true,
			AppAssetWebP: []byte("base-art"),
			GPU:          gpu,
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "app",
				Image: "steam:1",
				Env:   []corev1.EnvVar{{Name: LaunchCommandEnv, Value: "stale"}, {Name: "KEEP", Value: "1"}},
			}}}},
		},
	}
}

type catalogueFixture struct {
	dw        *dwfake.Clientset
	catalogue *Catalogue
	fetched   []string
}

func newCatalogueFixture(t *testing.T, objs ...runtime.Object) *catalogueFixture {
	t.Helper()
	f := &catalogueFixture{dw: dwfake.NewSimpleClientset(objs...)}
	f.catalogue = NewCatalogue(f.dw.DirewolfV1alpha1().Apps(libraryTestNS), fixtureExec(t, "testdata/catalogue"), "/games",
		CatalogueOptions{SteamBaseApp: "steam-base", HeroicBaseApp: "heroic-base"})
	f.catalogue.now = func() time.Time { return libraryTestNow }
	pngData := testPNG(t)
	f.catalogue.FetchArt = func(_ context.Context, u string) ([]byte, error) {
		f.fetched = append(f.fetched, u)
		if strings.Contains(u, "header.jpg") || strings.Contains(u, "wide") {
			return nil, errors.New("404")
		}
		return pngData, nil
	}
	return f
}

func (f *catalogueFixture) app(t *testing.T, name string) *v1alpha1types.App {
	t.Helper()
	app, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("App %s: %v", name, err)
	}
	return app
}

func (f *catalogueFixture) scan(t *testing.T) {
	t.Helper()
	if err := f.catalogue.Scan(context.Background(), libraryPod(0, true), catalogueArtBudget); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

func TestCatalogueSync(t *testing.T) {
	eightGi := resource.MustParse("8Gi")
	handMade := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: "firefox", Namespace: libraryTestNS}}
	uninstalled := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "steam-999", Namespace: libraryTestNS, UID: "u999",
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreSteam},
	}}
	f := newCatalogueFixture(t,
		baseApp("steam-base", &v1alpha1types.AppGPU{Memory: &eightGi}),
		baseApp("heroic-base", nil),
		handMade, uninstalled)
	f.scan(t)

	apps, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(apps.Items))
	for _, a := range apps.Items {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	want := []string{catalogueAppName(v1alpha1types.CatalogueStoreEpic, "Quail"), "firefox", "gog-1207658924", "heroic-base", "steam-1245620", "steam-570", "steam-base"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("Apps = %v, want %v (steam-999 uninstalled, hand-made firefox kept)", names, want)
	}

	dota := f.app(t, "steam-570")
	if dota.Labels[v1alpha1types.CatalogueLabel] != "steam" || dota.Annotations[catalogueIDAnnotation] != "570" {
		t.Errorf("steam-570 labels %v annotations %v", dota.Labels, dota.Annotations)
	}
	if dota.Spec.Title != "Dota 2" || dota.Spec.ID != catalogueMoonlightID("steam", "570") || dota.Spec.Hidden {
		t.Errorf("steam-570 spec title=%q id=%d hidden=%v", dota.Spec.Title, dota.Spec.ID, dota.Spec.Hidden)
	}
	if dota.Spec.GPU == nil || dota.Spec.GPU.Memory.Cmp(eightGi) != 0 {
		t.Errorf("steam-570 gpu = %+v, want the base App's 8Gi", dota.Spec.GPU)
	}
	env := dota.Spec.Template.Spec.Containers[0].Env
	wantEnv := []corev1.EnvVar{{Name: "KEEP", Value: "1"}, {Name: LaunchCommandEnv, Value: "steam -applaunch 570"}}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("steam-570 env = %+v, want %+v", env, wantEnv)
	}
	if len(dota.Spec.AppAssetWebP) == 0 || bytes.Equal(dota.Spec.AppAssetWebP, []byte("base-art")) {
		t.Error("steam-570 has no box art of its own")
	}
	ut := f.app(t, "gog-1207658924")
	if ut.Annotations[catalogueArtAnnotation] != "https://images.gog-statics.com/ut2004.jpg" {
		t.Errorf("UT2004 art from %q, want the GOG cover", ut.Annotations[catalogueArtAnnotation])
	}
	if hero := f.app(t, catalogueAppName("epic", "Quail")); !strings.Contains(hero.Spec.Template.Spec.Containers[0].Env[1].Value, "runner=legendary") {
		t.Errorf("Hades launch = %+v", hero.Spec.Template.Spec.Containers[0].Env)
	}

	// The user hides one game and gives it the whole card; the base App
	// moves to a new image.
	dota.Spec.Hidden = true
	dota.Spec.GPU = &v1alpha1types.AppGPU{}
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Update(context.Background(), dota, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	base := f.app(t, "steam-base")
	base.Spec.Template.Spec.Containers[0].Image = "steam:2"
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Update(context.Background(), base, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	fetchedBefore := len(f.fetched)
	f.scan(t)

	dota = f.app(t, "steam-570")
	if !dota.Spec.Hidden || dota.Spec.GPU == nil || dota.Spec.GPU.Memory != nil {
		t.Errorf("per-game settings not kept: hidden=%v gpu=%+v", dota.Spec.Hidden, dota.Spec.GPU)
	}
	if img := dota.Spec.Template.Spec.Containers[0].Image; img != "steam:2" {
		t.Errorf("image = %q, want the base App's new steam:2", img)
	}
	// Art comes once; failed URLs are not retried within catalogueArtRetry.
	if again := f.fetched[fetchedBefore:]; len(again) != 0 {
		t.Errorf("second scan fetched art again: %v", again)
	}
}

// A launcher whose base App is missing is not synced: its Apps stay, rather
// than being deleted as uninstalled.
func TestCatalogueSyncMissingBaseKeepsApps(t *testing.T) {
	heroicApp := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "gog-1", Namespace: libraryTestNS,
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreGOG},
	}}
	f := newCatalogueFixture(t, baseApp("steam-base", nil), heroicApp)
	if err := f.catalogue.Scan(context.Background(), libraryPod(0, true), catalogueArtBudget); err == nil {
		t.Error("Scan with a missing base App reported no error")
	}
	f.app(t, "gog-1")
	f.app(t, "steam-570")
}

// The Library controller scans while the Library runs and once more as it
// stops (after Steam has shut down), and a failing scan never blocks it.
func TestLibraryScansCatalogue(t *testing.T) {
	pod := libraryPod(2*DefaultLibraryIdleTimeout, true)
	f := newLibraryFixture(t, []runtime.Object{pod})
	cf := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil))
	scans := 0
	fixture := fixtureExec(t, "testdata/catalogue")
	cf.catalogue.Exec = func(ctx context.Context, ns, p, ctr string, command []string) (string, error) {
		scans++
		if f.exec.shutdowns == 0 {
			return "", errors.New("scanned before steam -shutdown")
		}
		return fixture(ctx, ns, p, ctr, command)
	}
	f.library.Catalogue = cf.catalogue

	if err := f.library.stop(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if scans != 1 || f.podExists(t) {
		t.Errorf("stop: %d scans, pod exists %v; want 1 scan after shutdown and the pod gone", scans, f.podExists(t))
	}
	cf.app(t, "steam-570")
	// It runs while the Library holds the Steam lock: no art.
	if len(cf.fetched) != 0 {
		t.Errorf("stop's scan fetched art: %v", cf.fetched)
	}

	// A periodic scan is rate limited.
	cf.catalogue.lastScan = libraryTestNow.Add(-time.Minute)
	if cf.catalogue.ScanDue() {
		t.Error("scan due a minute after the last one")
	}
	cf.catalogue.lastScan = libraryTestNow.Add(-catalogueScanInterval)
	if !cf.catalogue.ScanDue() {
		t.Error("scan not due after catalogueScanInterval")
	}
}

// App.spec.gpu becomes a per-Session ResourceClaim the pod references by
// name, on the game container and Wolf (NVENC on the same card).
func TestSessionPodGPUFromAppSpec(t *testing.T) {
	for _, tc := range []struct {
		gpu          string
		wantCapacity string
	}{
		{"gpu: {memory: 8Gi}", "8Gi"},
		{"gpu: {}", ""},
	} {
		raw, err := os.ReadFile("../../examples/steam.yaml")
		if err != nil {
			t.Fatal(err)
		}
		appPath := filepath.Join(t.TempDir(), "app.yaml")
		withGPU := strings.Replace(string(raw), "\nspec:\n", "\nspec:\n  "+tc.gpu+"\n", 1)
		if err = os.WriteFile(appPath, []byte(withGPU), 0o600); err != nil { //nolint:gosec // t.TempDir()
			t.Fatal(err)
		}
		_, k8s, sess, pod := reconcileFixtures(t, "../../examples/user.yaml", appPath)

		want := corev1.PodResourceClaim{Name: appGPUClaim, ResourceClaimName: new(gpuClaimName(sess.Name))}
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(c corev1.PodResourceClaim) bool { return reflect.DeepEqual(c, want) }) {
			t.Errorf("%s: pod resourceClaims = %+v, want %+v", tc.gpu, pod.Spec.ResourceClaims, want)
		}
		for _, ctr := range pod.Spec.Containers {
			has := slices.Contains(ctr.Resources.Claims, corev1.ResourceClaim{Name: appGPUClaim})
			if wantClaim := ctr.Name == "app" || ctr.Name == "wolf"; has != wantClaim {
				t.Errorf("%s: container %s claims GPU = %v, want %v", tc.gpu, ctr.Name, has, wantClaim)
			}
		}

		claim, err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Get(context.Background(), gpuClaimName(sess.Name), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s: %v", tc.gpu, err)
		}
		if !metav1.IsControlledBy(claim, sess) {
			t.Errorf("%s: claim owners = %+v, want the Session", tc.gpu, claim.OwnerReferences)
		}
		req := claim.Spec.Devices.Requests[0].Exactly
		if req.DeviceClassName != defaultGPUDeviceClass {
			t.Errorf("%s: device class %q", tc.gpu, req.DeviceClassName)
		}
		switch {
		case tc.wantCapacity == "" && req.Capacity != nil:
			t.Errorf("%s: whole card wanted, claim requests %+v", tc.gpu, req.Capacity)
		case tc.wantCapacity != "":
			got := req.Capacity.Requests[gpuClaimMemoryCapacity]
			if got.Cmp(resource.MustParse(tc.wantCapacity)) != 0 {
				t.Errorf("%s: claim memory = %s, want %s", tc.gpu, got.String(), tc.wantCapacity)
			}
		}
	}
}

// A claim left by an earlier Session of the same name is waited out, never
// adopted.
func TestGPUClaimFromEarlierSessionIsNotReused(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")
	app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	app = app.DeepCopy()
	app.Spec.GPU = &v1alpha1types.AppGPU{}
	if err := sc.AppInformer.GetIndexer().Update(app); err != nil {
		t.Fatal(err)
	}
	old := sess.DeepCopy()
	old.UID = "earlier"
	if _, err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Create(context.Background(), buildGPUClaim(app, old), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sc.reconcileGPUClaim(context.Background(), sess); err == nil {
		t.Error("reconcileGPUClaim adopted an earlier Session's claim")
	}
	if err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Delete(context.Background(), gpuClaimName(sess.Name), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sc.reconcileGPUClaim(context.Background(), sess); err != nil {
		t.Errorf("reconcileGPUClaim after GC: %v", err)
	}
	if _, err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Get(context.Background(), gpuClaimName(sess.Name), metav1.GetOptions{}); apierrors.IsNotFound(err) {
		t.Error("no claim created after the earlier one was gone")
	}
}

func writeFixture(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func scanFixtureErr(t *testing.T, dir string) error {
	t.Helper()
	out, err := fixtureExec(t, dir)(context.Background(), libraryTestNS, LibraryPodName, libraryContainer, catalogueScanCommand("/games"))
	if err != nil {
		return err
	}
	files, err := readTar(out)
	if err != nil {
		return err
	}
	_, err = parseInstalls(files)
	return err
}

const (
	gogInstalledRel = "home/.config/heroic/gog_store/installed.json"
	gogStore        = `{"installed": [{"appName": "1207658924", "is_dlc": false}]}`
)

// Files a Library user could plant: each must fail the scan (so nothing is
// deleted), never crash the operator or read as fewer games.
func TestCatalogueScanRejectsHostileFiles(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"deep nesting":       {"games/steamapps/appmanifest_9.acf": strings.Repeat(`"a"{`, 1000)},
		"oversized manifest": {"games/steamapps/appmanifest_9.acf": `"AppState" { "appid" "9" }` + strings.Repeat(" ", catalogueMaxManifestBytes)},
		"empty store":        {gogInstalledRel: ""},
	} {
		dir := t.TempDir()
		writeFixture(t, dir, map[string]string{"home/.keep": "", "games/.keep": ""})
		writeFixture(t, dir, files)
		if err := scanFixtureErr(t, dir); err == nil {
			t.Errorf("%s: scan succeeded", name)
		}
	}

	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"games/.keep": ""})
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(gogInstalledRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent", filepath.Join(dir, gogInstalledRel)); err != nil {
		t.Fatal(err)
	}
	if err := scanFixtureErr(t, dir); err == nil {
		t.Error("dangling symlinked store: scan succeeded")
	}

	deep := strings.Repeat(`"a"{`, vdfMaxDepth+1) + strings.Repeat("}", vdfMaxDepth+1)
	if _, err := parseVDF([]byte(deep)); err == nil {
		t.Error("parseVDF accepted nesting past vdfMaxDepth")
	}
	ok := strings.Repeat(`"a"{`, vdfMaxDepth) + strings.Repeat("}", vdfMaxDepth)
	if _, err := parseVDF([]byte(ok)); err != nil {
		t.Errorf("parseVDF at vdfMaxDepth: %v", err)
	}
	// Errors are logged: they must not quote the file, which may be a
	// symlink to something secret.
	if _, err := parseVDF([]byte("root:secrethash:19000")); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("parseVDF error quotes the file: %v", err)
	}
	if _, _, err := parseAppManifest([]byte(`"AppState" { "appid" "secret" }`)); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("parseAppManifest error quotes the file: %v", err)
	}
}

// A symlinked store is read through, not archived as an empty entry (which
// would read as no games and delete their Apps).
func TestCatalogueScanFollowsSymlinkedStore(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"games/.keep": "", "home/real.json": gogStore})
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(gogInstalledRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "home", "real.json"), filepath.Join(dir, gogInstalledRel)); err != nil {
		t.Fatal(err)
	}
	games := scanFixture(t, dir)
	if len(games) != 1 || games[0].ID != "1207658924" {
		t.Errorf("games = %+v, want the GOG game through the symlink", games)
	}
}

func TestParseAppManifestFilters(t *testing.T) {
	manifest := func(id, name string) []byte {
		return []byte(`"AppState" { "appid" "` + id + `" "name" "` + name + `" "StateFlags" "4" }`)
	}
	for _, tc := range []struct {
		id, name string
		ok, err  bool
	}{
		{"570", "Proton Pulse", true, false}, // a game, not a Proton build
		{"1493710", "Proton Experimental", false, false},
		{"2805730", "Proton 9.0", false, false},
		{"1628350", "Steam Linux Runtime 3.0 (sniper)", false, false},
		{"0", "Zero", false, true},
		{"0570", "Dota 2", false, true}, // would duplicate 570
	} {
		_, ok, err := parseAppManifest(manifest(tc.id, tc.name))
		if ok != tc.ok || (err != nil) != tc.err {
			t.Errorf("%s %q: ok=%v err=%v, want ok=%v err=%v", tc.id, tc.name, ok, err, tc.ok, tc.err)
		}
	}
	if got := truncateTitle("bad\x01ctl\n"); got != "badctl" {
		t.Errorf("truncateTitle kept control characters: %q", got)
	}
}

func TestPublicAddressOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"23.45.67.89:443":        true,
		"[2600:1f18::1]:443":     true,
		"10.0.0.1:443":           false,
		"127.0.0.1:443":          false,
		"169.254.169.254:443":    false,
		"[::1]:443":              false,
		"[fd00::1]:443":          false,
		"[::ffff:10.0.0.1]:443":  false,
		"100.64.0.1:443":         false,
		"198.18.0.1:443":         false,
		"[64:ff9b:1::a00:1]:443": false,
		"[::7f00:1]:443":         false,
		"[2001:0:1::1]:443":      false,
		"240.0.0.1:443":          false,
		"192.0.0.8:443":          false,
		"[64:ff9b::a00:1]:443":   false,
		"[2002:a00:1::]:443":     false,
		"0.0.0.0:443":            false,
	} {
		if got := publicAddressOnly("tcp", addr, nil) == nil; got != ok {
			t.Errorf("publicAddressOnly(%s) allowed = %v, want %v", addr, got, ok)
		}
	}
}

// A game whose art can't be had (none, or out of budget) still gets its App,
// with the base App's cover (the CRD rejects an empty one).
func TestCatalogueSyncWithoutArt(t *testing.T) {
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil))
	if err := f.catalogue.Scan(context.Background(), libraryPod(0, true), 0); err != nil {
		t.Fatal(err)
	}
	if len(f.fetched) != 0 {
		t.Errorf("a scan with no art budget fetched %v", f.fetched)
	}
	dota := f.app(t, "steam-570")
	if !bytes.Equal(dota.Spec.AppAssetWebP, []byte("base-art")) || dota.Annotations[catalogueArtAnnotation] != "" {
		t.Errorf("art = %q from %q, want the base App's", dota.Spec.AppAssetWebP, dota.Annotations[catalogueArtAnnotation])
	}
	// The next scan with a budget fetches it.
	f.scan(t)
	if dota = f.app(t, "steam-570"); dota.Annotations[catalogueArtAnnotation] == "" || bytes.Equal(dota.Spec.AppAssetWebP, []byte("base-art")) {
		t.Errorf("art not fetched on the next scan: from %q", dota.Annotations[catalogueArtAnnotation])
	}
}

// A hand-made App holding a game's name is never overwritten, and no art is
// fetched for a game that can't get an App.
func TestCatalogueSyncNameTakenByHandMadeApp(t *testing.T) {
	handMade := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: "steam-570", Namespace: libraryTestNS},
		Spec: v1alpha1types.AppSpec{Title: "mine"}}
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil), handMade)
	if err := f.catalogue.Scan(context.Background(), libraryPod(0, true), catalogueArtBudget); err == nil {
		t.Error("Scan reported no error for the name collision")
	}
	if app := f.app(t, "steam-570"); app.Spec.Title != "mine" || app.Labels[v1alpha1types.CatalogueLabel] != "" {
		t.Errorf("hand-made App changed: %+v", app)
	}
	for _, u := range f.fetched {
		if strings.Contains(u, "/570/") {
			t.Errorf("fetched art for the colliding game: %s", u)
		}
	}
	f.app(t, "steam-1245620") // the rest are still catalogued
}

// readTar takes regular files within the cap only, whatever the script let by.
func TestReadTarRejects(t *testing.T) {
	for name, hdr := range map[string]*tar.Header{
		"symlink":  {Name: "config/x.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"},
		"oversize": {Name: "config/x.json", Typeflag: tar.TypeReg, Size: catalogueMaxStoreBytes + 1},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(make([]byte, hdr.Size)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := readTar(buf.String()); err == nil {
			t.Errorf("%s: readTar accepted it", name)
		}
	}
}

// client-go only logs a failed stream copy, so a truncated stream must be
// turned into an error here, or a cut-off tar reads as fewer games.
func TestExecResultFailsOnOverflow(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		stdout, stderr := &cappedBuffer{max: 4}, &cappedBuffer{max: 4}
		over := map[string]*cappedBuffer{"stdout": stdout, "stderr": stderr}[stream]
		_, _ = io.Copy(over, strings.NewReader("abcdef"))
		if out, err := execResult(stdout, stderr, nil); err == nil {
			t.Errorf("%s over its cap: execResult = %q, nil", stream, out)
		}
	}
	if out, err := execResult(&cappedBuffer{max: 4}, &cappedBuffer{max: 4}, nil); err != nil || out != "" {
		t.Errorf("execResult within caps = %q, %v", out, err)
	}
}

// Two games on one Moonlight ID would launch the same App; the second is
// refused, not catalogued.
func TestCatalogueSyncRefusesDuplicateMoonlightID(t *testing.T) {
	seen := map[int]string{}
	var a, b string
	for i := 1; a == ""; i++ {
		id := strconv.Itoa(i)
		h := catalogueMoonlightID(v1alpha1types.CatalogueStoreGOG, id)
		if other, ok := seen[h]; ok {
			a, b = other, id
		}
		seen[h] = id
	}
	f := newCatalogueFixture(t, baseApp("heroic-base", nil))
	games := []catalogueGame{{Store: v1alpha1types.CatalogueStoreGOG, ID: a, Title: "A"}, {Store: v1alpha1types.CatalogueStoreGOG, ID: b, Title: "B"}}
	if err := f.catalogue.Sync(context.Background(), games, 0); err == nil {
		t.Error("Sync reported no error for the ID collision")
	}
	f.app(t, catalogueAppName(v1alpha1types.CatalogueStoreGOG, a))
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Get(context.Background(), catalogueAppName(v1alpha1types.CatalogueStoreGOG, b), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("second game on the same ID got an App (err %v)", err)
	}
}

// What the real executor runs: client-go returns nil after a failed write,
// and streamCapped must still fail.
func TestStreamCappedFailsOnOverflow(t *testing.T) {
	out, err := streamCapped(func(opts remotecommand.StreamOptions) error {
		_, _ = io.Copy(opts.Stdout, strings.NewReader(strings.Repeat("x", maxExecOutput+1)))
		return nil
	})
	if err == nil {
		t.Errorf("streamCapped over the cap = %d bytes, nil", len(out))
	}
}

// A Library path the scan user can't read fails the scan rather than reading
// as no games (which would delete their Apps); a folder that is not a Steam
// library (ext4's root-only lost+found) does not.
func TestCatalogueScanUnreadableDirFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything; permissions can't be tested")
	}
	for _, tc := range []struct {
		name    string
		vdf     func(dir string) map[string]string
		rel     string
		mode    os.FileMode
		wantErr bool
	}{
		{"listed library", steamappsVDF, "games/SteamLibrary", 0o000, true},
		{"listed library, not searchable", steamappsVDF, "games/SteamLibrary", 0o600, true},
		{"listed in config/ only", configVDF, "games/SteamLibrary", 0o000, true},
		{"listed in the pre-2021 format", oldFormatVDF, "games/SteamLibrary", 0o000, true},
		{"listed with spaces", spacedVDF, "games/Steam Lib", 0o000, true},
		{"listed with spaces, readable", spacedVDF, "games/Steam Lib", 0o755, false},
		{"unreadable libraryfolders.vdf", steamappsVDF, "home/.local/share/Steam/steamapps/libraryfolders.vdf", 0o000, true},
		{"unreadable config/ holding the list", configVDF, "home/.local/share/Steam/config", 0o000, true},
		{"heroic store dir", steamappsVDF, "home/.config/heroic/gog_store", 0o000, true},
		{"games volume", steamappsVDF, "games", 0o000, true},
		{"lost+found", steamappsVDF, "games/lost+found", 0o000, false},
		{"unlisted folder", steamappsVDF, "games/other", 0o000, false},
		{"relative path listed", relativeVDF, "games/other", 0o000, false},
	} {
		dir := t.TempDir()
		files := map[string]string{
			"games/SteamLibrary/steamapps/appmanifest_570.acf": `"AppState" { "appid" "570" "name" "Dota 2" "StateFlags" "4" }`,
			"games/Steam Lib/steamapps/appmanifest_571.acf":    `"AppState" { "appid" "571" "name" "Spaced" "StateFlags" "4" }`,
			"games/lost+found/.keep":                           "",
			"games/other/.keep":                                "",
			gogInstalledRel:                                    gogStore,
		}
		maps.Copy(files, tc.vdf(dir))
		writeFixture(t, dir, files)
		p := filepath.Join(dir, tc.rel)
		if err := os.Chmod(p, tc.mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) }) // restore, so TempDir can be removed
		if err := scanFixtureErr(t, dir); (err != nil) != tc.wantErr {
			t.Errorf("%s (%s at %o): scan err = %v, want error %v", tc.name, tc.rel, tc.mode, err, tc.wantErr)
		}
	}
}

const (
	steamappsVDFRel = "home/.local/share/Steam/steamapps/libraryfolders.vdf"
	configVDFRel    = "home/.local/share/Steam/config/libraryfolders.vdf"
)

func steamappsVDF(dir string) map[string]string {
	return map[string]string{steamappsVDFRel: libraryFolders(filepath.Join(dir, "games", "SteamLibrary"))}
}

func configVDF(dir string) map[string]string {
	return map[string]string{configVDFRel: libraryFolders(filepath.Join(dir, "games", "SteamLibrary"))}
}

func spacedVDF(dir string) map[string]string {
	return map[string]string{steamappsVDFRel: libraryFolders(filepath.Join(dir, "games", "Steam Lib"))}
}

func oldFormatVDF(dir string) map[string]string {
	return map[string]string{steamappsVDFRel: "\"LibraryFolders\"\n{\n\t\"TimeNextStatsReport\"\t\t\"0\"\n\t\"1\"\t\t\"" +
		filepath.Join(dir, "games", "SteamLibrary") + "\"\n}\n"}
}

// A relative (or Windows) path is no library on Linux; it must neither fail
// the scan nor hang it.
func relativeVDF(string) map[string]string {
	return map[string]string{steamappsVDFRel: libraryFolders("SteamLibrary", `D:\\SteamLibrary`)}
}

// libraryFolders is a libraryfolders.vdf as Steam writes it.
func libraryFolders(paths ...string) string {
	var sb strings.Builder
	sb.WriteString("\"libraryfolders\"\n{\n")
	for i, p := range paths {
		// With its apps block: numeric keys, as in the pre-2021 format,
		// whose values are not paths.
		fmt.Fprintf(&sb, "\t\"%d\"\n\t{\n\t\t\"path\"\t\t\"%s\"\n\t\t\"label\"\t\t\"\"\n"+
			"\t\t\"apps\"\n\t\t{\n\t\t\t\"570\"\t\t\"39871233412\"\n\t\t\t\"228980\"\t\t\"0\"\n\t\t}\n\t}\n", i, p)
	}
	sb.WriteString("}\n")
	return sb.String()
}

// Steam's library list: a dangling symlink, or one over the store cap, fails
// the scan; one exactly at the cap is read.
func TestCatalogueScanLibraryFoldersChecks(t *testing.T) {
	newDir := func() string {
		dir := t.TempDir()
		writeFixture(t, dir, map[string]string{"games/.keep": "", "home/.local/share/Steam/config/.keep": ""})
		return dir
	}

	dir := newDir()
	if err := os.Symlink("/nonexistent", filepath.Join(dir, configVDFRel)); err != nil {
		t.Fatal(err)
	}
	if err := scanFixtureErr(t, dir); err == nil {
		t.Error("dangling libraryfolders.vdf symlink: scan succeeded")
	}

	for size, wantErr := range map[int]bool{catalogueMaxStoreBytes: false, catalogueMaxStoreBytes + 1: true} {
		dir = newDir()
		vdf := libraryFolders(filepath.Join(dir, "games", "SteamLibrary"))
		writeFixture(t, dir, map[string]string{configVDFRel: vdf + strings.Repeat(" ", size-len(vdf))})
		if err := scanFixtureErr(t, dir); (err != nil) != wantErr {
			t.Errorf("libraryfolders.vdf of %d bytes: scan err = %v, want error %v", size, err, wantErr)
		}
	}
}

// The script must end itself before the exec gives up on it: a cancelled
// exec leaves its process running in the pod.
func TestCatalogueScanTimeoutWithinExecTimeout(t *testing.T) {
	secs, err := strconv.Atoi(catalogueScanTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(secs)*time.Second >= libraryExecTimeout {
		t.Errorf("catalogueScanTimeout %ss is not under libraryExecTimeout %s", catalogueScanTimeout, libraryExecTimeout)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 4}
	// io.Copy is how exec streams write: it must not find a way round Write.
	if _, err := io.Copy(b, strings.NewReader("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(b, strings.NewReader("e")); err == nil {
		t.Error("write past the cap succeeded")
	}
	if b.String() != "abcd" {
		t.Errorf("buffer = %q", b.String())
	}
}

func TestAppGPUMemoryMustBePositive(t *testing.T) {
	neg := resource.MustParse("-1Gi")
	app := &v1alpha1types.App{Spec: v1alpha1types.AppSpec{GPU: &v1alpha1types.AppGPU{Memory: &neg}}}
	if err := addAppGPUClaim(&corev1.PodSpec{}, app, &v1alpha1types.Session{}); err == nil {
		t.Error("negative gpu.memory accepted")
	}
}
