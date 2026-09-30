package main

import (
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/require"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

func sessionPod(namespace string) *api.PodSandbox {
	return &api.PodSandbox{
		Name:      "session-pod",
		Namespace: namespace,
		Labels:    map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue},
	}
}

func TestAdjustmentLabelledPod(t *testing.T) {
	adjust := (&plugin{}).adjustment(sessionPod("games"))
	require.NotNil(t, adjust)

	require.Len(t, adjust.GetLinux().GetDevices(), 1)
	dev := adjust.GetLinux().GetDevices()[0]
	require.Equal(t, "/dev/uinput", dev.Path)
	require.Equal(t, "c", dev.Type)
	require.Equal(t, int64(10), dev.Major)
	require.Equal(t, int64(223), dev.Minor)

	type rule struct {
		major  int64
		minor  int64 // -1 for wildcard
		access string
	}
	var got []rule
	for _, d := range adjust.GetLinux().GetResources().GetDevices() {
		if !d.Allow || d.Type != "c" {
			continue
		}
		r := rule{major: d.GetMajor().GetValue(), minor: -1, access: d.Access}
		if d.GetMinor() != nil {
			r.minor = d.GetMinor().GetValue()
		}
		got = append(got, r)
	}
	require.Contains(t, got, rule{13, -1, "rwm"})
	require.Contains(t, got, rule{241, -1, "rwm"})
	require.Contains(t, got, rule{10, 223, "rwm"})
}

func TestAdjustmentUntouchedPods(t *testing.T) {
	tests := map[string]struct {
		plugin *plugin
		pod    *api.PodSandbox
	}{
		"no labels": {
			plugin: &plugin{},
			pod:    &api.PodSandbox{Name: "other", Namespace: "games"},
		},
		"other labels only": {
			plugin: &plugin{},
			pod: &api.PodSandbox{Name: "other", Namespace: "games", Labels: map[string]string{
				"app": "direwolf-worker", "direwolf/user": "alice",
			}},
		},
		"wrong label value": {
			plugin: &plugin{},
			pod: &api.PodSandbox{Name: "other", Namespace: "games", Labels: map[string]string{
				v1alpha1types.SessionPodLabel: "false",
			}},
		},
		"labelled pod outside allowed namespaces": {
			plugin: &plugin{namespaces: []string{"games"}},
			pod:    sessionPod("default"),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, tc.plugin.adjustment(tc.pod))
		})
	}
}

func TestAdjustmentAllowedNamespace(t *testing.T) {
	p := &plugin{namespaces: []string{"other", "games"}}
	require.NotNil(t, p.adjustment(sessionPod("games")))
}

func TestParseNamespaces(t *testing.T) {
	require.Equal(t, []string{"other", "games"}, parseNamespaces(" other, games ,,"))
	require.Empty(t, parseNamespaces(""))
	require.Empty(t, parseNamespaces(" , "))
}

func TestNewPluginScope(t *testing.T) {
	_, err := newPlugin("", false)
	require.Error(t, err, "unscoped plugin must fail closed")
	_, err = newPlugin(" , ", false)
	require.Error(t, err, "blank namespace list must fail closed")
	_, err = newPlugin("games", true)
	require.Error(t, err, "flags are mutually exclusive")

	p, err := newPlugin("games", false)
	require.NoError(t, err)
	require.Equal(t, []string{"games"}, p.namespaces)

	p, err = newPlugin("", true)
	require.NoError(t, err)
	require.Empty(t, p.namespaces)
}
