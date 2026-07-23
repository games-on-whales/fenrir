package moonlight

import (
	"encoding/xml"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

type Responsible interface {
	GetStatusCode() int
}

type Response struct {
	XMLName       xml.Name `xml:"root"`
	StatusCode    int      `xml:"status_code,attr"`
	StatusMessage string   `xml:"status_message,attr,omitempty"`
}

func (r Response) GetStatusCode() int {
	return r.StatusCode
}

type ServerInfoResponse struct {
	Response   `xml:",inline"`
	ServerInfo `xml:",inline"`
}

// ServerInfo is the Root structure that represents the whole XML
type ServerInfo struct {
	Hostname               string       `xml:"hostname"`
	AppVersion             string       `xml:"appversion"`
	GfeVersion             string       `xml:"GfeVersion"`
	UniqueID               string       `xml:"uniqueid"`
	MaxLumaPixelsHEVC      int64        `xml:"MaxLumaPixelsHEVC"`
	ServerCodecModeSupport int          `xml:"ServerCodecModeSupport"`
	HTTPSPort              int          `xml:"HTTPSPort"`
	ExternalPort           int          `xml:"ExternalPort"`
	MAC                    string       `xml:"mac"`
	LocalIP                string       `xml:"LocalIP"`
	SupportedDisplayModes  DisplayModes `xml:"SupportedDisplayMode"`
	PairStatus             int          `xml:"PairStatus"`
	CurrentGame            string       `xml:"currentgame"`
	State                  string       `xml:"state"`
}

// DisplayModes is a wrapper for multiple display modes
type DisplayModes struct {
	Modes []DisplayMode `xml:"DisplayMode"`
}

// DisplayMode is the individual display mode struct
type DisplayMode struct {
	Width       int `xml:"Width"`
	Height      int `xml:"Height"`
	RefreshRate int `xml:"RefreshRate"`
}

type AppListResponse struct {
	Response `xml:",inline"`
	Apps     []App `xml:"App"`
}

type App struct {
	XMLName                  xml.Name `xml:"App"`
	direwolfv1alpha1.AppSpec `xml:",inline"`
}

type LaunchResponse struct {
	Response       `xml:",inline"`
	RTSPSessionURL string `xml:"sessionUrl0"`
	GameSession    int    `xml:"gamesession"`
}
