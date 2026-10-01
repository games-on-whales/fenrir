// Package fakeudev replicates wolf's fake-udev mechanism in Go.
//
// Wolf creates virtual input devices (joypads, mice, keyboards) through
// /dev/uinput. The kernel emits the corresponding uevents only in the
// initial network namespace, so processes inside the session pod never
// hear about them, and libudev consumers (SDL, Steam) ignore devices
// that have no entry in the udev database (/run/udev/data).
//
// In wolf's Docker runner this is solved by exec'ing the `fake-udev` C
// binary inside the app container, which:
//  1. writes the udev hwdb entries under /run/udev/data, and
//  2. broadcasts a synthetic "libudev" netlink message to the udev
//     multicast group (2) inside the container's network namespace.
//
// Containers in a Kubernetes pod share their network namespace, so the
// wolf-agent sidecar can do both jobs for the app container, provided
// /run/udev is a volume shared between the agent and app containers.
//
// Wolf creates the devices on the host, so their /dev/input nodes exist only
// in the host's devtmpfs. The agent recreates each one (mknod) in an emptyDir
// mounted at /dev/input in both the agent and the app container; the NRI
// plugin (cmd/nri-input) grants the pod's containers the input major in the
// device cgroup, which is what gates opening a node, whatever its path.
//
// Wire format reference:
// https://github.com/systemd/systemd/blob/main/src/libsystemd/sd-device/device-monitor.c
// and wolf's src/fake-udev.
package fakeudev

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	udevMonitorMagic = 0xfeedcafe
	// Multicast group used by udevd -> libudev listeners ("udev" source).
	udevEventGroup = 2
)

// murmurHash2 is the classic (non-A) MurmurHash2 used by
// systemd/libudev for filter_subsystem_hash (util_string_hash32).
func murmurHash2(data []byte, seed uint32) uint32 {
	const m = 0x5bd1e995
	const r = 24

	h := seed ^ uint32(len(data))
	for len(data) >= 4 {
		k := binary.LittleEndian.Uint32(data)
		k *= m
		k ^= k >> r
		k *= m
		h *= m
		h ^= k
		data = data[4:]
	}
	switch len(data) {
	case 3:
		h ^= uint32(data[2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[0])
		h *= m
	}
	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return h
}

// header mirrors systemd's monitor_netlink_header (40 bytes).
func makeHeader(propertiesLen int, subsystem, devtype string) []byte {
	buf := &bytes.Buffer{}
	buf.WriteString("libudev")
	buf.WriteByte(0)
	// magic is stored in network byte order
	_ = binary.Write(buf, binary.BigEndian, uint32(udevMonitorMagic))
	// sizes/offsets are native endian (x86_64: little endian)
	_ = binary.Write(buf, binary.LittleEndian, uint32(40)) // header_size
	_ = binary.Write(buf, binary.LittleEndian, uint32(40)) // properties_off
	_ = binary.Write(buf, binary.LittleEndian, uint32(propertiesLen))
	var subsystemHash, devtypeHash uint32
	if subsystem != "" {
		subsystemHash = murmurHash2([]byte(subsystem), 0)
	}
	if devtype != "" {
		devtypeHash = murmurHash2([]byte(devtype), 0)
	}
	// filter hashes are stored in network byte order
	_ = binary.Write(buf, binary.BigEndian, subsystemHash)
	_ = binary.Write(buf, binary.BigEndian, devtypeHash)
	_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // filter_tag_bloom_hi
	_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // filter_tag_bloom_lo
	return buf.Bytes()
}

// encodeProperties serializes the udev properties as NUL-separated
// KEY=VALUE pairs, ACTION first (mirrors wolf's std::map ordering, which
// is alphabetical; ACTION happens to sort first anyway).
func encodeProperties(props map[string]string) []byte {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf := &bytes.Buffer{}
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(props[k])
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// SendEvent broadcasts a synthetic libudev netlink event in the current
// network namespace. Requires CAP_NET_ADMIN. See fakeudev_linux.go.
func SendEvent(props map[string]string) error {
	subsystem := props["SUBSYSTEM"]
	if subsystem == "" {
		subsystem = "input"
	}
	payload := encodeProperties(props)
	msg := append(makeHeader(len(payload), subsystem, props["DEVTYPE"]), payload...)
	return sendNetlink(msg)
}

// WriteHwDbEntry writes a udev database entry (e.g. "c13:67") under
// baseDir (normally /run/udev/data) so libudev enumeration picks up the
// device properties (ID_INPUT_JOYSTICK etc.).
func WriteHwDbEntry(baseDir, filename string, content []string) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(baseDir, filepath.Base(filename))
	return os.WriteFile(path, []byte(strings.Join(content, "\n")), 0o644)
}

// RemoveHwDbEntry deletes a previously written udev database entry.
func RemoveHwDbEntry(baseDir, filename string) error {
	err := os.Remove(filepath.Join(baseDir, filepath.Base(filename)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ResolveDevNumbers fills in MAJOR/MINOR from sysfs when wolf could not
// stat the device node itself (it reports "0"/"0" in that case, because
// /dev/input is not mounted in the wolf container). DEVPATH is always
// present and sysfs is readable from any container.
func ResolveDevNumbers(props map[string]string) (major, minor string) {
	major, minor = props["MAJOR"], props["MINOR"]
	if major != "" && major != "0" {
		return major, minor
	}
	devpath := props["DEVPATH"]
	if devpath == "" {
		return major, minor
	}
	data, err := os.ReadFile(filepath.Join("/sys", devpath, "dev"))
	if err != nil {
		return major, minor
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
	if len(parts) != 2 {
		return major, minor
	}
	props["MAJOR"], props["MINOR"] = parts[0], parts[1]
	return parts[0], parts[1]
}

// inputMajor is the char major of /dev/input/* (evdev, js, mice). It is the
// only major CreateDeviceNode will create a node for.
const inputMajor = 13

// inputDevNamePrefix is the DEVNAME prefix of nodes CreateDeviceNode handles.
const inputDevNamePrefix = "/dev/input/"

// deviceNodePath maps a udev DEVNAME under /dev/input/ to its path under
// devDir. It refuses anything else (e.g. /dev/hidrawN, which does not live
// in /dev/input) and any name that is not a single path element.
func deviceNodePath(devDir, devname string) (string, error) {
	name, ok := strings.CutPrefix(devname, inputDevNamePrefix)
	if !ok || name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("not an input device node: %q", devname)
	}
	return filepath.Join(devDir, name), nil
}

// CreateDeviceNode creates the char device node for an input device event
// (DEVNAME=/dev/input/..., MAJOR=13) under devDir, replacing any stale node
// of the same name. The node is world read/writable: it lives only in the
// session pod's own volume. Requires CAP_MKNOD.
func CreateDeviceNode(devDir string, props map[string]string) error {
	path, err := deviceNodePath(devDir, props["DEVNAME"])
	if err != nil {
		return err
	}
	major, errMajor := strconv.ParseUint(props["MAJOR"], 10, 32)
	minor, errMinor := strconv.ParseUint(props["MINOR"], 10, 32)
	if err := errors.Join(errMajor, errMinor); err != nil {
		return fmt.Errorf("device numbers of %q: %w", props["DEVNAME"], err)
	}
	if major != inputMajor {
		return fmt.Errorf("refusing to create %q with major %d (only %d)", props["DEVNAME"], major, inputMajor)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale node: %w", err)
	}
	if err := makeCharNode(path, uint32(major), uint32(minor)); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// makeCharNode creates a 0666 char device node at path. The directory is
// writable by the app container, which can swap the new node for a symlink
// at any moment, so the mode must not be set by path. A variable so tests
// can check what is (not) created without CAP_MKNOD.
var makeCharNode = mknodChar

// ClearDir removes every entry directly in dir (not following symlinks).
// Wolf destroys a session's devices when its stream stops without sending
// unplug events, so their nodes and udev entries go with it, before the
// kernel hands their minors to another session's devices.
func ClearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RemoveDeviceNode deletes the node CreateDeviceNode made for props.
func RemoveDeviceNode(devDir string, props map[string]string) error {
	path, err := deviceNodePath(devDir, props["DEVNAME"])
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove device node: %w", err)
	}
	return nil
}

// IsInputDeviceNode reports whether props describes a /dev/input node,
// i.e. one CreateDeviceNode handles.
func IsInputDeviceNode(props map[string]string) bool {
	_, err := deviceNodePath("", props["DEVNAME"])
	return err == nil
}
