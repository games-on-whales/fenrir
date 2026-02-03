package wolfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/r3labs/sse/v2"
)

type Session struct {
	AppID             string         `json:"app_id,omitempty"`
	AudioChannelCount int            `json:"audio_channel_count"`
	ClientID          string         `json:"client_id,omitempty"` // omit, otherwise it throws 'Unhandled exception: stoull'
	ClientIP          string         `json:"client_ip"`
	ClientSettings    ClientSettings `json:"client_settings,omitempty"`
	VideoHeight       int            `json:"video_height"`
	VideoRefreshRate  int            `json:"video_refresh_rate"`
	VideoWidth        int            `json:"video_width"`

	AESKey string `json:"aes_key"`
	AESIV  string `json:"aes_iv"`

	RTSPFakeIP string `json:"rtsp_fake_ip,omitempty"` 

	// overrides
	H264GSTPipeline string `json:"h264_gst_pipeline,omitempty"`
	HEVCGSTPipeline string `json:"hevc_gst_pipeline,omitempty"`
	AV1GSTPipeline  string `json:"av1_gst_pipeline,omitempty"`
	OpusGSTPipeline string `json:"opus_gst_pipeline,omitempty"`
}

type Runner struct {
	Type   string `json:"type"`
	RunCmd string `json:"run_cmd,omitempty"`
}

type ClientSettings struct {
	ControllersOverride []string `json:"controllers_override"`
	//!TODO Float is lossy type. Possible to use decimal?
	HScrollAcceleration float64 `json:"h_scroll_acceleration"`
	MouseAcceleration   float64 `json:"mouse_acceleration"`
	RunGID              int     `json:"run_gid"`
	RunUID              int     `json:"run_uid"`
	VScrollAcceleration float64 `json:"v_scroll_acceleration"`
}
type Client interface {
	AddSession(ctx context.Context, session Session) (string, error)
	AddApp(ctx context.Context, app App) error
	StopSession(ctx context.Context, sessionID string) error
	ListSessions(ctx context.Context) ([]Session, error)
	ListApps(ctx context.Context) ([]App, error)
	SubscribeToEvents(ctx context.Context) (<-chan *sse.Event, error)
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

// GET /api/v1/apps
func (c *client) ListApps(ctx context.Context) ([]App, error) {
	u, err := url.JoinPath(c.apiURL, "/api/v1/apps")
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

	var appsResp AppsResponse
	if err := json.NewDecoder(resp.Body).Decode(&appsResp); err != nil {
		return nil, err
	}

	if !appsResp.Success {
		return nil, fmt.Errorf("failed to list apps: %s", appsResp.Error)
	}
	return appsResp.Apps, nil
}
// This is no longer used, I will probably remove it in the future
// POST /api/v1/apps/add
func (c *client) AddApp(ctx context.Context, app App) error {
	u, err := url.JoinPath(c.apiURL, "/api/v1/apps/add")
	if err != nil {
		return err
	}

	encodedApp, err := json.Marshal(app)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", u, bytes.NewBuffer(encodedApp))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var response Response
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}

	if !response.Success {
		return fmt.Errorf("failed to add app: %s", response.Error)
	}

	return nil
}

func (c *client) StopSession(ctx context.Context, sessionID string) error {
	type StopSessionRequest struct {
		SessionID string `json:"session_id"`
	}
	u, err := url.JoinPath(c.apiURL, "/api/v1/sessions/stop")
	if err != nil {
		return err
	}

	stopSessionReq := StopSessionRequest{
		SessionID: sessionID,
	}
	encodedStopSessionReq, err := json.Marshal(stopSessionReq)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", u, bytes.NewBuffer(encodedStopSessionReq))
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	var stopSessionResp Response
	if err := json.NewDecoder(resp.Body).Decode(&stopSessionResp); err != nil {
		return err
	} else if !stopSessionResp.Success {
		return fmt.Errorf("failed to stop session: %s", stopSessionResp.Error)
	}

	return nil
}

func (c *client) SubscribeToEvents(ctx context.Context) (<-chan *sse.Event, error) {
	events := make(chan *sse.Event)
	sseClient := sse.NewClient(c.apiURL+"/api/v1/events", func(cl *sse.Client) {
		cl.Connection = c.httpClient
	})

	err := sseClient.SubscribeChanRawWithContext(ctx, events)
	if err != nil {
		close(events)
		return nil, err
	}

	return events, nil
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

type App struct {
	ID                     string `json:"id"`
	Title                  string `json:"title"`
	SupportHDR             bool   `json:"support_hdr"`
	IconPNGPath            string `json:"icon_png_path"`
	StartVirtualCompositor bool   `json:"start_virtual_compositor"`
	StartAudioServer       bool   `json:"start_audio_server"`
	RenderNode             string `json:"render_node"`
	Runner                 Runner `json:"runner"`

	H264GSTPipeline string `json:"h264_gst_pipeline"`
	HEVCGSTPipeline string `json:"hevc_gst_pipeline"`
	AV1GSTPipeline  string `json:"av1_gst_pipeline"`
	OpusGSTPipeline string `json:"opus_gst_pipeline"`
}

type AppsResponse struct {
	Response `json:",inline"`
	Apps     []App `json:"apps"`
}

type WolfEventType string
const (
	PauseStreamEventType WolfEventType = "wolf::core::events::PauseStreamEvent"
)

type PauseStreamEvent struct {
	SessionID string `json:"session_id"`
}
