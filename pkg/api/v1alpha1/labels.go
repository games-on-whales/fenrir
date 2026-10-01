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

// LibraryPodLabel marks the operator's on-demand Library pod (Steam + Heroic
// desktop). moonlight-proxy refuses launches while a pod carrying it exists:
// the Library and game sessions share one Steam home, which has one writer.
//
// The value is always LibraryPodLabelValue.
const LibraryPodLabel = "direwolf/library"

// LibraryPodLabelValue is the value the operator sets for LibraryPodLabel.
const LibraryPodLabelValue = "true"
