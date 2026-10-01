package v1alpha1

// SessionPodLabel marks a pod as a Direwolf session worker. The operator sets
// it on every session pod, and the nri-input plugin grants input devices
// (/dev/input, /dev/hidraw, /dev/uinput) only to pods carrying it.
//
// The value is always SessionPodLabelValue; the operator's pod informer selects
// on it.
const SessionPodLabel = "direwolf/session"

// SessionPodLabelValue is the value the operator sets for SessionPodLabel.
const SessionPodLabelValue = "true"
