//go:build linux

package fakeudev

import (
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
