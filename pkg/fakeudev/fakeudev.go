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
// Wire format reference:
// https://github.com/systemd/systemd/blob/main/src/libsystemd/sd-device/device-monitor.c
// and wolf's src/fake-udev.
package fakeudev

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
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
