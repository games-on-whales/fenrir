package fakeudev

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCreateAndRemoveDeviceNode(t *testing.T) {
	dir := t.TempDir()
	props := map[string]string{"DEVNAME": "/dev/input/event67", "MAJOR": "13", "MINOR": "67"}
	// A stale node of the same name (from an earlier plug) is replaced.
	if err := os.WriteFile(filepath.Join(dir, "event67"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := CreateDeviceNode(dir, props)
	if errors.Is(err, unix.EPERM) {
		t.Skip("needs CAP_MKNOD")
	}
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "event67"), &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(st.Rdev) != 13 || unix.Minor(st.Rdev) != 67 {
		t.Errorf("node mode=%o rdev=%d:%d, want char 13:67", st.Mode, unix.Major(st.Rdev), unix.Minor(st.Rdev))
	}
	if perm := st.Mode & 0o777; perm != 0o666 {
		t.Errorf("node perm = %o, want 666", perm)
	}
	if err := RemoveDeviceNode(dir, props); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDeviceNode(dir, props); err != nil {
		t.Errorf("second remove: %v", err)
	}
}

// The node's mode is set through an fd on the node itself: whatever the app
// container swaps in after mknod (a symlink, its own file) is left alone.
func TestSetNodeModeRefusesSwappedNode(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "event5")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	dev := unix.Mkdev(13, 5)
	for _, path := range []string{link, victim} {
		if err := setNodeMode(path, dev); err == nil {
			t.Errorf("setNodeMode(%s) succeeded on a non-node", path)
		}
	}
	st, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("victim mode = %o, want 600", st.Mode().Perm())
	}
}
