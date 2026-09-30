//go:build !linux

package fakeudev

import "errors"

func sendNetlink(msg []byte) error {
	return errors.New("fake udev netlink events are only supported on linux")
}
