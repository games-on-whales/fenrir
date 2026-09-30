package v1alpha1

// SessionPodLabel marks a pod as a Direwolf session worker. The operator sets
// it on every session pod, and the nri-input plugin grants input devices
// (/dev/input, /dev/hidraw, /dev/uinput) only to pods carrying it.
//
// The value is always SessionPodLabelValue rather than the Session name, so a
// new Session for the same user/app does not change the Deployment's pod
// template and force a rollout.
const SessionPodLabel = "direwolf/session"

// SessionPodLabelValue is the value the operator sets for SessionPodLabel.
const SessionPodLabelValue = "true"
