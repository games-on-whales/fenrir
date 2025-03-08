package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"games-on-whales.github.io/direwolf/pkg/util"
	"github.com/r3labs/sse/v2"
)

func main() {
	serverCertPath := flag.String("tls-cert", "server.crt", "Path to server cert")
	serverKeyPath := flag.String("tls-key", "server.key", "Path to server key")
	serverPort := flag.Int("port", 443, "Port to listen on")
	wolfSocketPath := flag.String("socket", "/var/run/wolf.sock", "Path to wolf.sock")
	flag.Parse()

	log.Println("Starting wolf-agent")
	log.Println("TLS Cert:", *serverCertPath)
	log.Println("TLS Key:", *serverKeyPath)
	log.Println("Port:", *serverPort)
	log.Println("Wolf Socket:", *wolfSocketPath)

	client := UnixHTTPClient(*wolfSocketPath)

	// Start a thread to watch for the wolf.sock to appear
	var ready atomic.Bool
	go func() {
		for {
			// Check socket exists
			if info, err := os.Stat(*wolfSocketPath); err == nil && info != nil && info.Mode()&os.ModeSocket != 0 {
				conn, err := net.Dial("unix", *wolfSocketPath)
				if err == nil {
					defer conn.Close()
					ready.Store(true)
					log.Println("wolf.sock is ready")

					go func() {
						err = ListenToEvents(context.Background(), &client)
						if err != nil {
							panic(err)
						}
					}()

					return
				}
				log.Printf("Waiting for wolf.sock to accept connections: %v\n", err)
			} else if err == nil && info.Mode()&os.ModeSocket == 0 {
				log.Printf("wolf.sock is not a socket: %v\n", info.Mode())
				os.Exit(1)

			} else {
				log.Printf("Waiting for wolf.sock to appear: %v\n", err)
			}
			<-time.After(200 * time.Millisecond)
		}
	}()

	// Spin up HTTPS server with self-signed certificate to service the wolf.sock
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		log.Println("Received request:", r.Method, r.URL.Path)

		//!TODO: Use kubernetes metric.Filter or something to implement RBAC
		// authorization against the bearer token
		// Proxy the request to the wolf.sock
		url, err := url.JoinPath("http://", "wolf.sock", r.URL.Path)
		if err != nil {
			log.Println("Failed to join URL:", err)
			http.Error(w, fmt.Sprintf("Failed to join URL: %v", err), http.StatusInternalServerError)
			return
		}
		request, err := http.NewRequest(r.Method, url, r.Body)
		request.Proto = r.Proto
		request.ProtoMajor = r.ProtoMajor
		request.ProtoMinor = r.ProtoMinor
		request.TransferEncoding = r.TransferEncoding
		request.ContentLength = r.ContentLength
		if err != nil {
			log.Println("Failed to create proxy request:", err)
			http.Error(w, fmt.Sprintf("Failed to create proxy request: %v", err), http.StatusInternalServerError)
			return
		}
		request.Header = r.Header.Clone()

		// Send the request to the wolf.sock
		log.Println("Sending request to wolf.sock:", request.Method, request.URL.Path)
		response, err := client.Do(request)
		if err != nil {
			log.Println("Failed to send request to wolf.sock:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer response.Body.Close()

		// Write the response back to the client
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
		log.Println("Request completed:", response.StatusCode)
	})

	// Generate self-signed certificate and key
	cert, err := util.LoadCertificates(*serverCertPath, *serverKeyPath)
	if err != nil {
		panic(err)
	}

	// Start HTTPS server
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", *serverPort),
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}

	log.Printf("Listening on port %d\n", *serverPort)
	err = server.ListenAndServeTLS("", "")
	if err != nil {
		panic(err)
	}
}

func UnixHTTPClient(sockAddr string) http.Client {
	return http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", sockAddr)
			},
		},
	}
}

func ListenToEvents(
	ctx context.Context,
	client *http.Client,
) error {
	sseClient := sse.NewClient("http://wolf.sock/api/v1/events", func(c *sse.Client) {
		c.Connection = client
	})

	var events chan *sse.Event

	log.Println("Subscribing to events")
	err := sseClient.SubscribeChanRaw(events)
	if err != nil {
		return fmt.Errorf("failed to subscribe to events: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			if event == nil {
				continue
			}
			log.Printf("Received event")
			log.Printf("Event ID: %s", event.ID)
			log.Printf("Event Type: %s", event.Event)
			log.Printf("Event Data: %s", event.Data)
			log.Printf("Event Retry: %d", event.Retry)
			log.Printf("Event Comment: %v", event.Comment)
		}
	}
}
