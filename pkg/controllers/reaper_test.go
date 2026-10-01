package controllers

import (
	"testing"

	"games-on-whales.github.io/direwolf/pkg/moonlight"
)

// The reaper must not delete a Session that moonlight-proxy is still waiting
// on: any allowed --launch-timeout is below moonlight.ClientLaunchTimeout.
func TestUnstartedSessionTTLOutlastsLaunchTimeout(t *testing.T) {
	if unstartedSessionTTL <= moonlight.ClientLaunchTimeout {
		t.Errorf("unstartedSessionTTL = %s, must exceed %s", unstartedSessionTTL, moonlight.ClientLaunchTimeout)
	}
}
