package wolfapi

// TODO, go through the wolf code to mimick their structs
type Response struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// Session types
// TODO: use the shared settings from lobby?
type Session struct {
	AppID             string         `json:"app_id,omitempty"`
	AudioChannelCount int            `json:"audio_channel_count"`
	ClientID          string         `json:"client_id,omitempty"` // omit, otherwise it throws 'Unhandled exception: stoull'
	ClientIP          string         `json:"client_ip"`
	ClientSettings    ClientSettings `json:"client_settings"`
	VideoHeight       int            `json:"video_height"`
	VideoRefreshRate  int            `json:"video_refresh_rate"`
	VideoWidth        int            `json:"video_width"`

	AESKey string `json:"aes_key"`
	AESIV  string `json:"aes_iv"`

	RTSPFakeIP string `json:"rtsp_fake_ip,omitempty"`

	// overrides the app because we only need the session to be created
	// will take over other app specs in the next commit
	H264GSTPipeline string `json:"h264_gst_pipeline,omitempty"`
	HEVCGSTPipeline string `json:"hevc_gst_pipeline,omitempty"`
	AV1GSTPipeline  string `json:"av1_gst_pipeline,omitempty"`
	OpusGSTPipeline string `json:"opus_gst_pipeline,omitempty"`
}

type ClientSettings struct {
	ControllersOverride []string `json:"controllers_override"`
	// TODO: Float is a lossy type. Consider using decimal?
	HScrollAcceleration      float64 `json:"h_scroll_acceleration"`
	MouseAcceleration        float64 `json:"mouse_acceleration"`
	RunGID                   int     `json:"run_gid"`
	RunUID                   int     `json:"run_uid"`
	VScrollAcceleration      float64 `json:"v_scroll_acceleration"`
	MotionControllerOverride string  `json:"motion_controller_override,omitempty"`
}

type App struct {
	Title string `json:"title"`
	ID    string `json:"id"`
}

type AppsResponse struct {
	Success bool   `json:"success"`
	Apps    []App  `json:"apps"`
	Error   string `json:"error"`
}
type StopSessionRequest struct {
	SessionID string `json:"session_id"`
}

type SessionsResponse struct {
	Response `json:",inline"`
	Sessions []Session `json:"sessions"`
}

type AddSessionResponse struct {
	Response  `json:",inline"`
	SessionID string `json:"session_id"`
}

type Runner struct {
	Type   string `json:"type"`
	RunCmd string `json:"run_cmd,omitempty"`
}

// WolfEventType are the main wolf events that we want
// the wolf-agent to log
type WolfEventType string

const (
	PauseStreamEventType   WolfEventType = "wolf::core::events::PauseStreamEvent"
	ResumeStreamEventType  WolfEventType = "wolf::core::events::ResumeStreamEvent"
	StreamSessionEventType WolfEventType = "wolf::core::events::StreamSession"
	VideoSessionEventType  WolfEventType = "wolf::core::events::VideoSession"
	AudioSessionEventType  WolfEventType = "wolf::core::events::AudioSession"
)

type PauseStreamEvent struct {
	SessionID string `json:"session_id"`
}

type ResumeStreamEvent struct {
	SessionID string `json:"session_id"`
}

type StreamSessionEvent struct {
	ClientID string `json:"client_id"`
	AppID    string `json:"app_id"`
}

type VideoSessionEvent struct {
	SessionID string `json:"session_id"`
}

type AudioSessionEvent struct {
	SessionID string `json:"session_id"`
}

// Lobby types
type Lobby struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type LobbiesResponse struct {
	Response `json:",inline"`
	Lobbies  []Lobby `json:"lobbies"`
}

type LobbyVideoSettings struct {
	Width                   int    `json:"width"`
	Height                  int    `json:"height"`
	RefreshRate             int    `json:"refresh_rate"`
	WaylandRenderNode       string `json:"wayland_render_node"`
	RunnerRenderNode        string `json:"runner_render_node"`
	VideoProducerBufferCaps string `json:"video_producer_buffer_caps"`
}

type LobbyAudioSettings struct {
	ChannelCount int `json:"channel_count"`
}

type LobbyCreateRequest struct {
	ProfileID              string             `json:"profile_id"`
	Name                   string             `json:"name"`
	IconPNGPath            string             `json:"icon_png_path,omitempty"`
	PinRequired            bool               `json:"pin_required"`
	Pin                    []int              `json:"pin"`
	MultiUser              bool               `json:"multi_user"`
	StopWhenEveryoneLeaves bool               `json:"stop_when_everyone_leaves"`
	RunnerStateFolder      string             `json:"runner_state_folder"`
	Runner                 Runner             `json:"runner"`
	ClientSettings         ClientSettings     `json:"client_settings"`
	VideoSettings          LobbyVideoSettings `json:"video_settings"`
	AudioSettings          LobbyAudioSettings `json:"audio_settings"`
	ConnectedSessions      []string           `json:"connected_sessions"`
}

type LobbyCreateResponse struct {
	Response `json:",inline"`
	LobbyID  string `json:"lobby_id"`
}

type JoinLobbyRequest struct {
	LobbyID            string `json:"lobby_id"`
	MoonlightSessionID string `json:"moonlight_session_id"`
	Pin                []int  `json:"pin,omitempty"`
}

type LeaveLobbyRequest struct {
	LobbyID            string `json:"lobby_id"`
	MoonlightSessionID string `json:"moonlight_session_id"`
}

type StopLobbyRequest struct {
	LobbyID string `json:"lobby_id"`
	Pin     []int  `json:"pin,omitempty"`
}
