package wolfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type Session struct {
	AppID             string         `json:"app_id"`
	AudioChannelCount int            `json:"audio_channel_count"`
	ClientID          string         `json:"client_id"`
	ClientIP          string         `json:"client_ip"`
	ClientSettings    ClientSettings `json:"client_settings"`
	VideoHeight       int            `json:"video_height"`
	VideoRefreshRate  int            `json:"video_refresh_rate"`
	VideoWidth        int            `json:"video_width"`

	AESKey string `json:"aes_key"`
	AESIV  string `json:"aes_iv"`
}

type ClientSettings struct {
	ControllersOverride []string `json:"controllers_override"`
	HScrollAcceleration float64  `json:"h_scroll_acceleration"`
	MouseAcceleration   float64  `json:"mouse_acceleration"`
	RunGID              int      `json:"run_gid"`
	RunUID              int      `json:"run_uid"`
	VScrollAcceleration float64  `json:"v_scroll_acceleration"`
}
type Client interface {
	AddSession(ctx context.Context, session Session) (string, error)
	ListSessions(ctx context.Context) ([]Session, error)
}

type client struct {
	apiURL     string
	httpClient *http.Client
}

func NewClient(
	apiURL string,
	httpClient *http.Client,
) Client {
	return &client{
		apiURL:     apiURL,
		httpClient: httpClient,
	}
}

// POST /api/v1/sessions/add
func (c *client) AddSession(
	ctx context.Context,
	session Session,
) (string, error) {
	u, err := url.JoinPath(c.apiURL, "/api/v1/sessions/add")
	if err != nil {
		return "", err
	}

	encodedSession, err := json.Marshal(session)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", u, bytes.NewBuffer(encodedSession))
	if err != nil {
		return "", err
	}

	// FORCE HTTP/1.0 (this disables chunked encoding automatically)
	req.Proto = "HTTP/1.0"
	req.ProtoMajor = 1
	req.ProtoMinor = 0
	req.TransferEncoding = []string{"identity"}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var addSessionResp AddSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&addSessionResp); err != nil {
		return "", err
	}

	if !addSessionResp.Success {
		return "", fmt.Errorf("failed to add session: %s", addSessionResp.Error)
	}

	return addSessionResp.SessionID, nil
}

// GET /api/v1/sessions
func (c *client) ListSessions(ctx context.Context) ([]Session, error) {
	u, err := url.JoinPath(c.apiURL, "/api/v1/sessions")
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var sessionsResp SessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&sessionsResp); err != nil {
		return nil, err
	}

	if !sessionsResp.Success {
		return nil, fmt.Errorf("failed to list sessions: %s", sessionsResp.Error)
	}

	return sessionsResp.Sessions, nil
}

type Response struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type SessionsResponse struct {
	Response `json:",inline"`
	Sessions []Session `json:"sessions"`
}

type AddSessionResponse struct {
	Response  `json:",inline"`
	SessionID string `json:"session_id"`
}
