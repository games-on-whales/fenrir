//go:build !linux

package fakeudev

import "errors"

func sendNetlink(msg []byte) error {
	return errors.New("fake udev netlink events are only supported on linux")
}

func mknodChar(path string, major, minor uint32) error {
	return errors.New("device nodes are only supported on linux")
}
