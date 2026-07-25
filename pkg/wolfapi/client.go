package wolfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/r3labs/sse/v2"
)

type Client interface {
	AddSession(ctx context.Context, session Session) (string, error)
	StopSession(ctx context.Context, sessionID string) error
	ListSessions(ctx context.Context) ([]Session, error)
	SubscribeToEvents(ctx context.Context) (<-chan *sse.Event, error)
	ListLobbies(ctx context.Context) ([]Lobby, error)
	CreateLobby(ctx context.Context, req LobbyCreateRequest) (*LobbyCreateResponse, error)
	JoinLobby(ctx context.Context, req JoinLobbyRequest) error
	LeaveLobby(ctx context.Context, req LeaveLobbyRequest) error
	StopLobby(ctx context.Context, req StopLobbyRequest) error
}

type client struct {
	apiURL     string
	httpClient *http.Client
}

func NewClient(apiURL string, httpClient *http.Client) Client {
	return &client{
		apiURL:     apiURL,
		httpClient: httpClient,
	}
}

// do performs an HTTP request and decodes the JSON response.
func (c *client) do(ctx context.Context, method, path string, body, result interface{}) error {
	u, err := url.JoinPath(c.apiURL, path)
	if err != nil {
		return fmt.Errorf("building URL for %s: %w", path, err)
	}

	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body for %s: %w", path, err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return fmt.Errorf("creating request for %s: %w", path, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request for %s: %w", path, err)
	}
	defer resp.Body.Close()

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response for %s: %w", path, err)
		}
	}

	return nil
}

func (c *client) get(ctx context.Context, path string, result interface{}) error {
	return c.do(ctx, http.MethodGet, path, nil, result)
}

func (c *client) post(ctx context.Context, path string, body, result interface{}) error {
	return c.do(ctx, http.MethodPost, path, body, result)
}

func (c *client) AddSession(ctx context.Context, session Session) (string, error) {
	var resp AddSessionResponse
	if err := c.post(ctx, "/api/v1/sessions/add", session, &resp); err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("failed to add session: %s", resp.Error)
	}
	return resp.SessionID, nil
}

func (c *client) ListSessions(ctx context.Context) ([]Session, error) {
	var resp SessionsResponse
	if err := c.get(ctx, "/api/v1/sessions", &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("failed to list sessions: %s", resp.Error)
	}
	return resp.Sessions, nil
}

func (c *client) StopSession(ctx context.Context, sessionID string) error {
	req := StopSessionRequest{SessionID: sessionID}
	var resp Response
	if err := c.post(ctx, "/api/v1/sessions/stop", req, &resp); err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to stop session: %s", resp.Error)
	}
	return nil
}

func (c *client) SubscribeToEvents(ctx context.Context) (<-chan *sse.Event, error) {
	u, err := url.JoinPath(c.apiURL, "/api/v1/events")
	if err != nil {
		return nil, fmt.Errorf("building events URL: %w", err)
	}

	events := make(chan *sse.Event)
	sseClient := sse.NewClient(u, func(cl *sse.Client) {
		cl.Connection = c.httpClient
	})

	if err := sseClient.SubscribeChanRawWithContext(ctx, events); err != nil {
		close(events)
		return nil, fmt.Errorf("subscribing to events: %w", err)
	}

	return events, nil
}

func (c *client) ListLobbies(ctx context.Context) ([]Lobby, error) {
	var resp LobbiesResponse
	if err := c.get(ctx, "/api/v1/lobbies", &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("failed to list lobbies: %s", resp.Error)
	}
	return resp.Lobbies, nil
}

func (c *client) CreateLobby(ctx context.Context, req LobbyCreateRequest) (*LobbyCreateResponse, error) {
	var resp LobbyCreateResponse
	if err := c.post(ctx, "/api/v1/lobbies/create", req, &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("failed to create lobby: %s", resp.Error)
	}
	return &resp, nil
}

func (c *client) JoinLobby(ctx context.Context, req JoinLobbyRequest) error {
	var resp Response
	if err := c.post(ctx, "/api/v1/lobbies/join", req, &resp); err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to join lobby: %s", resp.Error)
	}
	return nil
}

func (c *client) LeaveLobby(ctx context.Context, req LeaveLobbyRequest) error {
	var resp Response
	if err := c.post(ctx, "/api/v1/lobbies/leave", req, &resp); err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to leave lobby: %s", resp.Error)
	}
	return nil
}

func (c *client) StopLobby(ctx context.Context, req StopLobbyRequest) error {
	var resp Response
	if err := c.post(ctx, "/api/v1/lobbies/stop", req, &resp); err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to stop lobby: %s", resp.Error)
	}
	return nil
}
