package moonlight

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

func appServer(t *testing.T, apps ...*v1alpha1types.App) *RESTServer {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, a := range apps {
		if err := indexer.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	return &RESTServer{AppLister: generic.NewLister[*v1alpha1types.App](indexer).Namespaced(testNamespace)}
}

func testApp(name string, id int, hidden bool, asset []byte) *v1alpha1types.App {
	return &v1alpha1types.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       v1alpha1types.AppSpec{Title: name, ID: id, Hidden: hidden, AppAssetWebP: asset},
	}
}

// Hidden Apps (e.g. the catalogue's base Apps) are not listed, and can't be
// launched or looked up by ID either.
func TestHiddenAppsAreNotListed(t *testing.T) {
	s := appServer(t, testApp("shown", 1, false, nil), testApp("steam-base", 2, true, nil))

	rec := httptest.NewRecorder()
	s.appListHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/applist", http.NoBody))
	body := rec.Body.String()
	if !strings.Contains(body, "<AppTitle>shown</AppTitle>") || strings.Contains(body, "steam-base") {
		t.Errorf("applist = %s, want only the shown App", body)
	}
	if strings.Contains(body, "Hidden") || strings.Contains(body, "GPU") {
		t.Errorf("applist leaks App fields Moonlight doesn't know: %s", body)
	}
	if _, err := s.getAppByID("2"); err == nil {
		t.Error("getAppByID found the hidden App")
	}
	if _, err := s.getAppByID("1"); err != nil {
		t.Errorf("getAppByID(1): %v", err)
	}
}

// The catalogue stores the box art it fetches (JPEG from Steam's CDN) as is;
// Moonlight gets it as PNG like a WebP asset.
func TestAppAssetServesJPEGAsPNG(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 6, 9)), nil); err != nil {
		t.Fatal(err)
	}
	s := appServer(t, testApp("game", 7, false, jpg.Bytes()))

	rec := httptest.NewRecorder()
	s.appAssetHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/appasset?appid=7", http.NoBody))
	img, err := png.Decode(rec.Body)
	if err != nil {
		t.Fatalf("appasset is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 6 || b.Dy() != 9 {
		t.Errorf("appasset is %v, want 6x9", b)
	}
}
