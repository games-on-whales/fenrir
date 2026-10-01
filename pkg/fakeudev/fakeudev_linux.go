//go:build linux

package fakeudev

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func sendNetlink(msg []byte) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return fmt.Errorf("create netlink socket: %w", err)
	}
	defer unix.Close(fd)

	addr := &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: udevEventGroup}
	if err := unix.Bind(fd, addr); err != nil {
		return fmt.Errorf("bind netlink socket: %w", err)
	}
	if err := unix.Sendto(fd, msg, 0, addr); err != nil {
		return fmt.Errorf("send netlink message: %w", err)
	}
	return nil
}

func mknodChar(path string, major, minor uint32) error {
	// Mkdev of two 32-bit numbers fits in 64 bits; int is 64-bit on every
	// platform Wolf runs on.
	dev := int(unix.Mkdev(major, minor)) //nolint:gosec // see above
	if err := unix.Mknod(path, unix.S_IFCHR|0o666, dev); err != nil {
		return fmt.Errorf("mknod: %w", err)
	}
	// mknod's mode is filtered by the umask.
	return setNodeMode(path, uint64(dev)) //nolint:gosec // dev came from Mkdev
}

// setNodeMode makes the char node dev at path 0666. It opens path without
// following a symlink, checks it is that node, and chmods through the fd,
// never by path (see makeCharNode).
func setNodeMode(path string, dev uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open new node: %w", err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat new node: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Rdev != dev {
		return errors.New("node was replaced before its mode was set")
	}
	// chmod on an O_PATH fd's /proc link changes that inode, not a path.
	if err := unix.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), 0o666); err != nil {
		return fmt.Errorf("chmod new node: %w", err)
	}
	return nil
}
