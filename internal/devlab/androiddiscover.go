package devlab

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// adbDevice is one row of `adb devices -l`.
type adbDevice struct {
	Serial    string
	State     string // device | unauthorized | offline | ...
	Model     string // the -l model: field, underscores for dashes
	Product   string
	Transport string // usb:<path>, or empty over TCP
}

// emulator rows are never lab devices: the lab measures physical phones only.
func (d adbDevice) emulator() bool { return strings.HasPrefix(d.Serial, "emulator-") }

// parseAdbDevices reads `adb devices -l`, emulators dropped.
func parseAdbDevices(out string) []adbDevice {
	var devices []adbDevice
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := adbDevice{Serial: fields[0], State: fields[1]}
		for _, f := range fields[2:] {
			key, value, ok := strings.Cut(f, ":")
			if !ok {
				continue
			}
			switch key {
			case "model":
				d.Model = value
			case "product":
				d.Product = value
			case "usb":
				d.Transport = "usb:" + value
			}
		}
		if !d.emulator() {
			devices = append(devices, d)
		}
	}
	return devices
}

func (l *Lab) adbDevices(ctx context.Context) ([]adbDevice, error) {
	out, err := l.run(ctx, 20*time.Second, "adb", "devices", "-l")
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("adb devices -l: %s", stderrTail(out))
	}
	return parseAdbDevices(out.Stdout), nil
}

// parseGetprop reads a full `getprop` dump: `[key]: [value]` lines.
func parseGetprop(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "]: [")
		if !ok || !strings.HasPrefix(key, "[") || !strings.HasSuffix(value, "]") {
			continue
		}
		props[key[1:]] = value[:len(value)-1]
	}
	return props
}

// parseWmSize prefers the override size (what apps render at, 1080x2400 on
// an S20 at FHD+) over the physical panel.
func parseWmSize(out string) string {
	var physical, override string
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Physical size":
			physical = strings.TrimSpace(value)
		case "Override size":
			override = strings.TrimSpace(value)
		}
	}
	if override != "" {
		return override
	}
	return physical
}

// androidIdentity is what add and scan read off an attached phone.
type androidIdentity struct {
	Model, Manufacturer, Device, OS, Resolution string
	SDK                                         int
}

func (l *Lab) androidIdentity(ctx context.Context, serial string) (androidIdentity, error) {
	out, err := l.run(ctx, 20*time.Second, "adb", "-s", serial, "shell", "getprop")
	if err != nil {
		return androidIdentity{}, err
	}
	if out.Code != 0 {
		return androidIdentity{}, fmt.Errorf("adb -s %s shell getprop: %s", serial, stderrTail(out))
	}
	props := parseGetprop(out.Stdout)
	id := androidIdentity{
		Model:        props["ro.product.model"],
		Manufacturer: props["ro.product.manufacturer"],
		Device:       props["ro.product.device"],
		OS:           props["ro.build.version.release"],
	}
	id.SDK, _ = strconv.Atoi(props["ro.build.version.sdk"])
	if wm, err := l.run(ctx, 10*time.Second, "adb", "-s", serial, "shell", "wm", "size"); err == nil && wm.Code == 0 {
		id.Resolution = parseWmSize(wm.Stdout)
	}
	return id, nil
}
