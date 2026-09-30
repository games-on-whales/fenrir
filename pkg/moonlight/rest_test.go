package moonlight

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"games-on-whales.github.io/direwolf/pkg/util"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected addr %T", l.Addr())
	}
	return addr.Port
}

func waitListening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("port %d never started listening", port)
}

// A pair request waiting for its PIN must not keep Run from returning, nor
// keep the HTTPS listener (/launch) serving while the HTTP one drains.
func TestRunShutdownIsBoundedAndConcurrent(t *testing.T) {
	certPEM, keyPEM, err := util.GenerateEphemeralCert("ECC")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	port, securePort, pinPort := freePort(t), freePort(t), freePort(t)
	s := NewRESTServer(NewPairingManager(cert, nil), nil, nil, nil, nil, nil, nil, RESTServerOptions{
		Port:            port,
		SecurePort:      securePort,
		Cert:            cert,
		PinPage:         PinPageOptions{Port: pinPort},
		shutdownTimeout: 300 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	waitListening(t, port)
	waitListening(t, securePort)
	waitListening(t, pinPort)

	// Hold a phase 1 request open over HTTP; it blocks until a PIN arrives, so
	// the HTTP server's graceful shutdown can't complete.
	reqCtx, cancelReq := context.WithCancel(t.Context())
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/pair?uniqueid=x&salt=%x&clientcert=%x", port, make([]byte, 16), heldCertPEM(t)), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.manager.PendingPairings()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("HTTP pair request never became pending")
		}
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	cancel()

	// The other listeners must stop accepting promptly, not after HTTP drains.
	for _, p := range []int{securePort, pinPort} {
		for {
			c, err := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				break
			}
			c.Close()
			if time.Since(start) > 200*time.Millisecond {
				t.Fatalf("port %d still accepting while HTTP shutdown waits on a pair request", p)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its shutdown timeout")
	}
}

func heldCertPEM(t *testing.T) []byte {
	t.Helper()
	certPEM, _, err := util.GenerateEphemeralCert("ECC")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(certPEM)
}
