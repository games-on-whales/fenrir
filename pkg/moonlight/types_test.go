package moonlight

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSerialize(t *testing.T) {
	root := ServerInfoResponse{
		Response: Response{
			StatusCode: 200,
		},
		ServerInfo: ServerInfo{
			Hostname:               "Wolf",
			AppVersion:             "7.1.431.-1",
			GfeVersion:             "3.23.0.74",
			UniqueID:               "dd7c60f6-4b88-4ef1-be07-eeec72f96080",
			MaxLumaPixelsHEVC:      1869449984,
			ServerCodecModeSupport: 65793,
			HttpsPort:              47984,
			ExternalPort:           47989,
			MAC:                    "00:00:00:00:00:00",
			LocalIP:                "10.129.2.254",
			SupportedDisplayModes: DisplayModes{
				Modes: []DisplayMode{
					{1280, 720, 120}, {1280, 720, 60}, {1280, 720, 30},
					{1920, 1080, 120}, {1920, 1080, 60}, {1920, 1080, 30},
					{2560, 1440, 120}, {2560, 1440, 90}, {2560, 1440, 60},
					{3840, 2160, 120}, {3840, 2160, 90}, {3840, 2160, 60},
					{7680, 4320, 120}, {7680, 4320, 90}, {7680, 4320, 60},
				},
			},
			PairStatus:  0,
			CurrentGame: "1",
			State:       "SUNSHINE_SERVER_BUSY",
		},
	}

	// Hmm wonder why this bit isnt included
	// <?xml version="1.0" encoding="utf-8"?>

	expected := `
<root status_code="200">
  <hostname>Wolf</hostname>
  <appversion>7.1.431.-1</appversion>
  <GfeVersion>3.23.0.74</GfeVersion>
  <uniqueid>dd7c60f6-4b88-4ef1-be07-eeec72f96080</uniqueid>
  <MaxLumaPixelsHEVC>1869449984</MaxLumaPixelsHEVC>
  <ServerCodecModeSupport>65793</ServerCodecModeSupport>
  <HttpsPort>47984</HttpsPort>
  <ExternalPort>47989</ExternalPort>
  <mac>00:00:00:00:00:00</mac>
  <LocalIP>10.129.2.254</LocalIP>
  <SupportedDisplayMode>
    <DisplayMode>
      <Width>1280</Width>
      <Height>720</Height>
      <RefreshRate>120</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>1280</Width>
      <Height>720</Height>
      <RefreshRate>60</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>1280</Width>
      <Height>720</Height>
      <RefreshRate>30</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>1920</Width>
      <Height>1080</Height>
      <RefreshRate>120</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>1920</Width>
      <Height>1080</Height>
      <RefreshRate>60</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>1920</Width>
      <Height>1080</Height>
      <RefreshRate>30</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>2560</Width>
      <Height>1440</Height>
      <RefreshRate>120</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>2560</Width>
      <Height>1440</Height>
      <RefreshRate>90</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>2560</Width>
      <Height>1440</Height>
      <RefreshRate>60</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>3840</Width>
      <Height>2160</Height>
      <RefreshRate>120</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>3840</Width>
      <Height>2160</Height>
      <RefreshRate>90</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>3840</Width>
      <Height>2160</Height>
      <RefreshRate>60</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>7680</Width>
      <Height>4320</Height>
      <RefreshRate>120</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>7680</Width>
      <Height>4320</Height>
      <RefreshRate>90</RefreshRate>
    </DisplayMode>
    <DisplayMode>
      <Width>7680</Width>
      <Height>4320</Height>
      <RefreshRate>60</RefreshRate>
    </DisplayMode>
  </SupportedDisplayMode>
  <PairStatus>0</PairStatus>
  <currentgame>1</currentgame>
  <state>SUNSHINE_SERVER_BUSY</state>
</root>
`
	expected = strings.TrimSpace(expected)

	serialized, err := xml.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatalf("Error serializing XML: %v", err)
	}

	if diff := cmp.Diff(expected, string(serialized)); diff != "" {
		t.Errorf("Unexpected XML serialization (-want +got):\n%s", diff)
	}
}
