package controllers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

const (
	// LibraryPodName is the one Library pod per namespace. A fixed name makes
	// concurrent visits converge on one Create (the rest get AlreadyExists).
	LibraryPodName = "direwolf-library"

	libraryContainer = "library"
	// libraryAuthSecret holds the password Selkies' nginx demands (basic
	// auth, user libraryAuthUser) on the pod's ports. The operator's proxy
	// adds it; nothing else knows it, so reaching the pod IP directly gets
	// nowhere even where NetworkPolicy isn't enforced.
	libraryAuthSecret = "direwolf-library-auth"
	libraryAuthKey    = "password"
	libraryAuthUser   = "abc"
	// libraryUnixUser is the image's desktop (and Steam) user, not to be
	// confused with the basic-auth name above.
	libraryUnixUser = "abc"
	// libraryHTTPPort is Selkies' plain-HTTP port in linuxserver images.
	libraryHTTPPort = 3000
	// libraryHome is HOME for the image's abc user, so the Steam home PVC
	// holds ~/.local/share/Steam and Heroic's ~/.config/heroic.
	libraryHome = "/config"

	// libraryActivityAnnotation holds the last time (RFC3339) a browser
	// talked to the Library through the operator. It lives on the pod, not
	// in memory, so any operator replica can serve the page while only the
	// leader runs the idle check, and a restart does not reset the clock.
	libraryActivityAnnotation = "direwolf/library-last-activity"

	// DefaultLibraryIdleTimeout is how long the Library may go without
	// browser traffic before it is stopped (if nothing is downloading).
	DefaultLibraryIdleTimeout = 15 * time.Minute

	libraryCheckInterval = time.Minute
	libraryExecTimeout   = 30 * time.Second
	// libraryShutdownTimeout bounds `steam -shutdown` plus the wait for the
	// Steam process to exit, so Steam can flush the shared home cleanly.
	libraryShutdownTimeout = 90 * time.Second

	libraryBusyReason = "A game is streaming right now. Steam can only run in one place at a time, " +
		"so the Library opens once the game session has ended."
)

// heroicDownloadQueue is Heroic's persisted download manager store
// (electron-store "download-manager" under ~/.config/heroic/store). The
// download in progress stays in "queue" until it finishes.
const heroicDownloadQueue = libraryHome + "/.config/heroic/store/download-manager.json"

// PodExecutor runs command in a container and returns its stdout. A non-zero
// exit is an error.
type PodExecutor func(ctx context.Context, namespace, pod, container string, command []string) (string, error)

// maxExecOutput caps what an exec may return: the Library's desktop user can
// write the files the catalogue scan reads, and the operator has 256Mi.
const maxExecOutput = 32 << 20

// cappedBuffer collects output and fails writes past max bytes. It wraps
// rather than embeds bytes.Buffer: an embedded ReadFrom or WriteString would
// let io.Copy and friends skip the cap. The overflow is also recorded:
// client-go only logs a failed stdout/stderr copy and returns no error, so
// the caller must check Overflowed or get silently truncated output.
type cappedBuffer struct {
	buf        bytes.Buffer
	max        int
	overflowed bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.overflowed || b.buf.Len()+len(p) > b.max {
		b.overflowed = true
		return 0, fmt.Errorf("exec output over %d bytes", b.max)
	}
	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer only fails on OOM
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// Overflowed reports whether output was dropped past max.
func (b *cappedBuffer) Overflowed() bool { return b.overflowed }

// execResult is an exec's stdout, or an error if it failed or either stream
// went over its cap (truncated output must never pass for the whole of it).
func execResult(stdout, stderr *cappedBuffer, err error) (string, error) {
	if err == nil && (stdout.Overflowed() || stderr.Overflowed()) {
		err = fmt.Errorf("exec output over its cap (stdout %d, stderr %d bytes)", stdout.max, stderr.max)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// NewPodExecutor execs through the API server (pods/exec).
func NewPodExecutor(config *rest.Config, client kubernetes.Interface) PodExecutor {
	return func(ctx context.Context, namespace, pod, container string, command []string) (string, error) {
		req := client.CoreV1().RESTClient().Post().
			Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: container,
				Command:   command,
				Stdout:    true,
				Stderr:    true,
			}, scheme.ParameterCodec)
		executor, err := remotecommand.NewWebSocketExecutor(config, "GET", req.URL().String())
		if err != nil {
			return "", fmt.Errorf("creating exec stream: %w", err)
		}
		return streamCapped(func(opts remotecommand.StreamOptions) error {
			return executor.StreamWithContext(ctx, opts)
		})
	}
}

// streamCapped runs an exec stream into capped buffers and returns its
// stdout through execResult.
func streamCapped(stream func(remotecommand.StreamOptions) error) (string, error) {
	stdout := &cappedBuffer{max: maxExecOutput}
	stderr := &cappedBuffer{max: 64 << 10}
	return execResult(stdout, stderr, stream(remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr}))
}

type LibraryControllerOptions struct {
	Image string
	// HomePVC is the shared Steam home (also mounted by Steam game sessions),
	// mounted at /config.
	HomePVC string
	// GamesPVC is the shared game library, mounted at GamesPath.
	GamesPVC  string
	GamesPath string

	// The Library runs on the session node (talos04), next to the local game
	// library volume.
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration

	IdleTimeout time.Duration
}

// LibraryController owns the on-demand Library pod: it is created by a visit
// to the Library page (LibraryServer), and stopped once the browser has been
// gone for IdleTimeout and nothing is downloading.
//
// The Library and game sessions are mutually exclusive (Steam's single-writer
// lock on the shared home): the Library is not started while a Session or a
// session pod exists, and moonlight-proxy refuses launches while the Library
// pod exists. Both sides check, create, then check again, so two racing
// starts can't both survive.
type LibraryController struct {
	Namespace     string
	K8sClient     kubernetes.Interface
	SessionClient v1alpha1client.SessionInterface
	PodInformer   generic.Informer[*corev1.Pod]
	Exec          PodExecutor
	// Catalogue, if set, is scanned while the Library runs and as it stops.
	Catalogue *Catalogue

	controller generic.Controller[*corev1.Pod]
	now        func() time.Time
	LibraryControllerOptions
}

func NewLibraryController(
	namespace string,
	k8sClient kubernetes.Interface,
	sessionClient v1alpha1client.SessionInterface,
	podInformer generic.Informer[*corev1.Pod],
	exec PodExecutor,
	options LibraryControllerOptions, //nolint:gocritic // built once at startup
) *LibraryController {
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = DefaultLibraryIdleTimeout
	}
	c := &LibraryController{
		Namespace:                namespace,
		K8sClient:                k8sClient,
		SessionClient:            sessionClient,
		PodInformer:              podInformer,
		Exec:                     exec,
		now:                      time.Now,
		LibraryControllerOptions: options,
	}
	c.controller = generic.NewController(podInformer, c.Reconcile, generic.ControllerOptions{
		Name:    "library-controller",
		Workers: 1,
	})
	return c
}

func (c *LibraryController) Run(ctx context.Context) error {
	return c.controller.Run(ctx) //nolint:wrapcheck // the caller names the controller
}

// Reconcile runs the idle check on the Library pod and requeues itself, so
// the check keeps running without pod events.
func (c *LibraryController) Reconcile(namespace, name string, pod *corev1.Pod) error {
	// Load bearing: a missing pod arrives as a typed nil.
	if pod == nil || name != LibraryPodName || pod.DeletionTimestamp != nil {
		return nil
	}
	ctx := context.Background()
	if c.Catalogue != nil && podReady(pod) && c.Catalogue.ScanDue() {
		c.scanCatalogue(ctx, pod, catalogueArtBudget)
	}
	requeueAfter, err := c.reconcileIdle(ctx, pod)
	if c.Catalogue != nil && requeueAfter > catalogueScanInterval {
		requeueAfter = catalogueScanInterval
	}
	if requeueAfter > 0 {
		c.controller.EnqueueAfter(namespace, name, requeueAfter)
	}
	return err
}

// reconcileIdle stops the Library once it is idle, and otherwise says when to
// look again.
func (c *LibraryController) reconcileIdle(ctx context.Context, pod *corev1.Pod) (time.Duration, error) {
	// A failed or evicted Library serves nobody but would hold the Steam lock
	// (and block launches) until it idled out.
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		klog.Infof("Library pod is %s, removing it", pod.Status.Phase)
		return 0, c.stop(ctx, pod)
	}

	idleFor := c.now().Sub(libraryLastActivity(pod))
	if idleFor < c.IdleTimeout {
		return c.IdleTimeout - idleFor, nil
	}

	// A Library that never came up (or is crash-looping) serves nobody and
	// can't be downloading; only a ready one is asked.
	if podReady(pod) {
		downloading, err := c.downloadsInProgress(ctx, pod)
		if err != nil {
			// Fail safe: never stop it over a check we couldn't make.
			klog.Errorf("Library idle check failed, keeping it up: %v", err)
			return libraryCheckInterval, nil
		}
		if downloading != "" {
			klog.V(2).Infof("Library idle for %s but still downloading (%s)", idleFor.Round(time.Second), downloading)
			return libraryCheckInterval, nil
		}
	}

	// The informer copy may predate a browser coming back; ask the API server
	// before killing Steam under them.
	fresh, err := c.K8sClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to re-read Library pod: %w", err)
	}
	if fresh.UID != pod.UID {
		return 0, nil
	}
	if idleFor = c.now().Sub(libraryLastActivity(fresh)); idleFor < c.IdleTimeout {
		return c.IdleTimeout - idleFor, nil
	}

	klog.Infof("Library idle for %s with no downloads, stopping it", idleFor.Round(time.Second))
	return 0, c.stop(ctx, fresh)
}

// stop asks Steam to shut down cleanly (it writes to the shared home), waits
// for it to exit, then deletes the pod. A failed shutdown still deletes the
// pod: SIGTERM is the fallback, and leaving it up would hold the Steam lock.
func (c *LibraryController) stop(ctx context.Context, pod *corev1.Pod) error {
	if podReady(pod) {
		shutdownCtx, cancel := context.WithTimeout(ctx, libraryShutdownTimeout)
		_, err := c.Exec(shutdownCtx, pod.Namespace, pod.Name, libraryContainer, steamShutdownCommand)
		cancel()
		if err != nil {
			klog.Errorf("Steam did not shut down cleanly, deleting the Library pod anyway: %v", err)
		}
		// Last look, now Steam has written its manifests out. No art: the
		// Steam lock is held until the pod is gone; the next run fetches it.
		if c.Catalogue != nil {
			c.scanCatalogue(ctx, pod, 0)
		}
	}
	err := c.K8sClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pod.UID},
	})
	if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
		return fmt.Errorf("failed to delete Library pod: %w", err)
	}
	return nil
}

// scanCatalogue syncs the catalogue from the Library. A failed scan changes
// nothing and holds up the Library (or its shutdown) for a bounded time only.
func (c *LibraryController) scanCatalogue(ctx context.Context, pod *corev1.Pod, artBudget time.Duration) {
	if err := c.Catalogue.Scan(ctx, pod, artBudget); err != nil {
		klog.Errorf("Catalogue scan failed: %v", err)
	}
}

// steamShutdownCommand runs `steam -shutdown` as the desktop user (only if
// Steam is running: on a stopped Steam it would start one) and waits for the
// Steam process to exit. Plain sh and /proc only, so it needs no extra tools.
var steamShutdownCommand = []string{"sh", "-c", `
running() { grep -qx steam /proc/[0-9]*/comm 2>/dev/null; }
running || exit 0
HOME=` + libraryHome + ` s6-setuidgid abc /usr/bin/steam -shutdown >/dev/null 2>&1 &
i=0
while running; do
  i=$((i+1)); [ "$i" -ge 60 ] && { echo "steam still running after 60s" >&2; exit 1; }
  sleep 1
done
`}

// downloadsInProgress returns what is still downloading, or "" when nothing
// is: any entry in a Steam library's steamapps/downloading, or anything in
// Heroic's download queue.
func (c *LibraryController) downloadsInProgress(ctx context.Context, pod *corev1.Pod) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, libraryExecTimeout)
	defer cancel()

	out, err := c.Exec(ctx, pod.Namespace, pod.Name, libraryContainer, steamDownloadsCommand(c.GamesPath))
	if err != nil {
		return "", fmt.Errorf("listing Steam downloads: %w", err)
	}
	if entries := nonEmptyLines(out); len(entries) > 0 {
		return "Steam: " + strings.Join(entries, ", "), nil
	}

	out, err = c.Exec(ctx, pod.Namespace, pod.Name, libraryContainer,
		[]string{"sh", "-c", `[ ! -f "$1" ] || cat "$1"`, "sh", heroicDownloadQueue})
	if err != nil {
		return "", fmt.Errorf("reading Heroic download queue: %w", err)
	}
	queued, err := heroicQueueLen([]byte(out))
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", heroicDownloadQueue, err)
	}
	if queued > 0 {
		return fmt.Sprintf("Heroic: %d queued", queued), nil
	}
	return "", nil
}

// steamDownloadsCommand lists every entry of steamapps/downloading in the
// default Steam library and in library folders on the games volume (at its
// root or one directory down, e.g. <games>/SteamLibrary).
func steamDownloadsCommand(gamesPath string) []string {
	return []string{"sh", "-c", `
for d in "$1/.local/share/Steam/steamapps/downloading" "$2/steamapps/downloading" "$2"/*/steamapps/downloading; do
  [ -d "$d" ] && ls -A "$d" | sed "s|^|$d/|"
done
exit 0
`, "sh", libraryHome, gamesPath}
}

func nonEmptyLines(s string) []string {
	var lines []string
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// heroicQueueLen counts the downloads in Heroic's download manager store. A
// missing (empty) store is an empty queue; an unparseable one is an error, so
// a store read mid-write never passes for "nothing downloading".
func heroicQueueLen(data []byte) (int, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, nil
	}
	var store struct {
		Queue []json.RawMessage `json:"queue"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return 0, fmt.Errorf("decoding download queue: %w", err)
	}
	return len(store.Queue), nil
}

// libraryLastActivity is the latest of the pod's creation and the last
// browser traffic recorded on it.
func libraryLastActivity(pod *corev1.Pod) time.Time {
	last := pod.CreationTimestamp.Time
	if v, ok := pod.Annotations[libraryActivityAnnotation]; ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(last) {
			last = t
		}
	}
	return last
}

// RecordActivity stamps the Library pod with the time of the last browser
// traffic (see libraryActivityAnnotation).
func (c *LibraryController) RecordActivity(ctx context.Context, at time.Time) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{libraryActivityAnnotation: at.UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("encoding activity patch: %w", err)
	}
	_, err = c.K8sClient.CoreV1().Pods(c.Namespace).Patch(ctx, LibraryPodName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("patching Library pod: %w", err)
	}
	return nil
}

// EnsurePod returns the Library pod, creating it unless a game is running, in
// which case it returns a non-empty reason and no pod.
func (c *LibraryController) EnsurePod(ctx context.Context) (*corev1.Pod, string, error) {
	pods := c.K8sClient.CoreV1().Pods(c.Namespace)
	pod, err := pods.Get(ctx, LibraryPodName, metav1.GetOptions{})
	if err == nil {
		return pod, "", nil
	}
	if !errors.IsNotFound(err) {
		return nil, "", fmt.Errorf("failed to get Library pod: %w", err)
	}

	if busy, checkErr := c.gameRunning(ctx); checkErr != nil || busy {
		return nil, libraryBusyReasonIf(busy), checkErr
	}
	if _, authErr := c.AuthPassword(ctx); authErr != nil {
		return nil, "", authErr
	}

	pod, err = pods.Create(ctx, c.buildPod(), metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		// A concurrent visit created it; that one ran the lock checks.
		pod, err = pods.Get(ctx, LibraryPodName, metav1.GetOptions{})
		if err != nil {
			return nil, "", fmt.Errorf("failed to get Library pod: %w", err)
		}
		return pod, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("failed to create Library pod: %w", err)
	}
	klog.Info("Started the Library pod")

	// A launch may have passed moonlight-proxy's check before our pod
	// existed. Whichever side looks second sees the other and backs off.
	busy, err := c.gameRunning(ctx)
	if err != nil || busy {
		delErr := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}})
		if delErr != nil && !errors.IsNotFound(delErr) {
			klog.Errorf("Failed to back out the Library pod: %v", delErr)
		}
		return nil, libraryBusyReasonIf(busy), err
	}
	return pod, "", nil
}

func libraryBusyReasonIf(busy bool) string {
	if busy {
		return libraryBusyReason
	}
	return ""
}

// gameRunning reports whether a game session holds Steam: any Session, or a
// session pod still terminating after its Session is gone. Read from the API
// server, not the informer, so a Session created a moment ago counts.
func (c *LibraryController) gameRunning(ctx context.Context) (bool, error) {
	sessions, err := c.SessionClient.List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return false, fmt.Errorf("failed to list sessions: %w", err)
	}
	if len(sessions.Items) > 0 {
		return true, nil
	}
	pods, err := c.K8sClient.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue,
		}).String(),
		Limit: 1,
	})
	if err != nil {
		return false, fmt.Errorf("failed to list session pods: %w", err)
	}
	return len(pods.Items) > 0, nil
}

func (c *LibraryController) buildPod() *corev1.Pod {
	volumes := []corev1.Volume{
		{Name: "steam-home", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.HomePVC},
		}},
		{Name: "games", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.GamesPVC},
		}},
		// Steam's CEF and Heroic's Electron fall over on the 64Mi default.
		{Name: "dshm", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: new(resource.MustParse("1Gi")),
			},
		}},
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LibraryPodName,
			Namespace: c.Namespace,
			Labels: map[string]string{
				v1alpha1types.LibraryPodLabel: v1alpha1types.LibraryPodLabelValue,
			},
		},
		Spec: corev1.PodSpec{
			// The desktop has no business talking to the Kubernetes API.
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			NodeSelector:                 c.NodeSelector,
			Tolerations:                  c.Tolerations,
			Volumes:                      volumes,
			Containers: []corev1.Container{{
				Name:  libraryContainer,
				Image: c.Image,
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: libraryHTTPPort, Protocol: corev1.ProtocolTCP}},
				Env: []corev1.EnvVar{
					{Name: "CUSTOM_USER", Value: libraryAuthUser},
					{Name: "PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: libraryAuthSecret},
						Key:                  libraryAuthKey,
					}}},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(libraryHTTPPort)},
					},
					PeriodSeconds: 2,
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "steam-home", MountPath: libraryHome},
					{Name: "games", MountPath: c.GamesPath},
					{Name: "dshm", MountPath: "/dev/shm"},
				},
			}},
		},
	}
}

// AuthPassword returns the Library's basic-auth password (see
// libraryAuthSecret), creating it on first use. It is never rotated by the
// operator: a running pod keeps the value it started with.
func (c *LibraryController) AuthPassword(ctx context.Context) (string, error) {
	secrets := c.K8sClient.CoreV1().Secrets(c.Namespace)
	secret, err := secrets.Get(ctx, libraryAuthSecret, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			return "", fmt.Errorf("generating Library password: %w", err)
		}
		secret, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: libraryAuthSecret, Namespace: c.Namespace},
			Data:       map[string][]byte{libraryAuthKey: []byte(hex.EncodeToString(raw))},
		}, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			secret, err = secrets.Get(ctx, libraryAuthSecret, metav1.GetOptions{})
		}
	}
	if err != nil {
		return "", fmt.Errorf("failed to get Library auth secret: %w", err)
	}
	password := string(secret.Data[libraryAuthKey])
	if password == "" {
		return "", fmt.Errorf("library auth secret %s has no %q key", libraryAuthSecret, libraryAuthKey)
	}
	return password, nil
}

// ValidateLibraryGamesPath rejects a games mount the pod spec can't hold: it
// must be absolute and clear of the Library's other mounts.
func ValidateLibraryGamesPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) == "/" {
		return fmt.Errorf("%q must be an absolute path below /", p)
	}
	p = path.Clean(p)
	for _, other := range []string{libraryHome, "/dev/shm"} {
		if p == other || strings.HasPrefix(p, other+"/") || strings.HasPrefix(other, p+"/") {
			return fmt.Errorf("%q overlaps the Library's %s mount", p, other)
		}
	}
	return nil
}
