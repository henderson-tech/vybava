package claudeguards

import (
	"fmt"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// machine:device-leased - perflab leases a physical phone to one token
// holder (internal/devlab). On 2026-10-01 two copies of one agent drove the
// same phone and each rebuilt and reinstalled the other's variant, so a
// night of measurements compared nothing. A lease only helps if nothing
// else touches the phone: while one is held, a raw adb, devicectl, xctrace,
// go-ios or Appium command naming the device is refused, the holder's own
// included. The holder drives it through perflab, whose passthroughs
// (`perflab device shell|screencap|pull`) check the token and take the
// device lock. No escape hatch: the passthrough is as cheap as the raw call.
// ---------------------------------------------------------------------------

// deviceTools are the commands that address one phone by a handle.
var deviceTools = map[string]bool{
	"adb": true, "devicectl": true, "xctrace": true, "ios": true, "appium": true,
	"idevicesyslog": true, "ideviceinstaller": true, "idevicescreenshot": true, "ideviceinfo": true,
}

// adbHostVerbs never touch a device.
var adbHostVerbs = map[string]bool{
	"devices": true, "version": true, "help": true, "start-server": true, "connect": true,
	"disconnect": true, "pair": true, "mdns": true, "keygen": true, "--version": true, "-h": true, "--help": true,
}

// adbServerVerbs disrupt every attached device, the leased one included.
var adbServerVerbs = map[string]bool{"kill-server": true, "reconnect": true, "usb": true, "tcpip": true, "root": true, "unroot": true}

// adbValueFlags take the next word as their value.
var adbValueFlags = map[string]bool{"-s": true, "-t": true, "-H": true, "-P": true, "-L": true}

// deviceLeases lists the held leases (a test seam; the real one reads
// perflab's state dir without a lock and fails open).
var deviceLeases = func() []devlab.HeldLease {
	lab, err := devlab.Open()
	if err != nil {
		return nil
	}
	held, err := lab.HeldLeases()
	if err != nil {
		return nil
	}
	return held
}

// deviceUse is what one segment does to devices.
type deviceUse struct {
	tool    string   // the device tool it runs, "" when none
	handles []string // every word that may name a device
	bareADB bool     // adb on a device verb with no -s / ANDROID_SERIAL
	server  string   // an adb server verb (kill-server, …)
}

func baseName(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// segmentDeviceUse reads one command segment and the assignments that
// prefix it.
func segmentDeviceUse(assign []string, seg string) deviceUse {
	var u deviceUse
	toks := append(append([]string{}, assign...), shellseg.Fields(seg)...)
	i := 0
	if i < len(toks) && toks[i] == "export" {
		i++
	}
	serialSet := false
	for ; i < len(toks) && shellseg.AssignPrefix.MatchString(toks[i]); i++ {
		name, val, _ := strings.Cut(toks[i], "=")
		u.handles = append(u.handles, val)
		serialSet = serialSet || name == "ANDROID_SERIAL"
	}
	rest := toks[i:]
	if len(rest) == 0 {
		return u
	}
	// perflab validates the token itself.
	first := baseName(rest[0])
	if first == "perflab" || (first == "vybava" && len(rest) > 1 && rest[1] == "perflab") {
		return deviceUse{}
	}
	at := -1
	for j, t := range rest {
		b := baseName(t)
		if deviceTools[b] || (strings.HasPrefix(b, "idevice") && b != "idevice") {
			at = j
			break
		}
		if j == 0 && !shellseg.Runners[b] && b != "xcrun" && b != "bunx" && b != "npx" {
			break
		}
	}
	if at < 0 {
		return u
	}
	u.tool = baseName(rest[at])
	args := rest[at+1:]
	for _, a := range args {
		if _, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "-") {
			u.handles = append(u.handles, v)
		}
		u.handles = append(u.handles, a)
	}
	if u.tool == "adb" {
		named, verb := serialSet, ""
		for j := 0; j < len(args); j++ {
			a := args[j]
			if a == "-s" {
				named = true
			}
			if adbValueFlags[a] {
				j++
				continue
			}
			if strings.HasPrefix(a, "-") && !adbHostVerbs[a] {
				continue
			}
			verb = a
			break
		}
		switch {
		case adbServerVerbs[verb]:
			u.server = verb
		case verb != "" && !adbHostVerbs[verb] && !named:
			u.bareADB = true
		}
	}
	return u
}

func guardDeviceLeased(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if cmd == "" {
		return nil
	}
	var uses []deviceUse
	for _, c := range shellseg.LocalCommands(cmd) {
		if textOnly(c.Text) {
			continue
		}
		if u := segmentDeviceUse(c.Assign, c.Text); u.tool != "" || len(u.handles) > 0 {
			uses = append(uses, u)
		}
	}
	if len(uses) == 0 {
		return nil
	}
	held := deviceLeases()
	if len(held) == 0 {
		return nil
	}
	for _, u := range uses {
		for _, h := range held {
			for _, alias := range h.Aliases {
				if alias == h.DeviceID {
					continue // ledger ids are short words; only hardware handles match
				}
				for _, w := range u.handles {
					if strings.EqualFold(w, alias) {
						what := "`" + orTool(u.tool) + "`"
						return deny("machine:device-leased", deviceLeasedMsg(what+" names "+alias, h), "")
					}
				}
			}
			if h.Platform == devlab.PlatformAndroid && u.server != "" {
				return deny("machine:device-leased", deviceLeasedMsg("`adb "+u.server+"` restarts adb for every attached phone", h), "")
			}
			if h.Platform == devlab.PlatformAndroid && u.bareADB {
				return deny("machine:device-leased", deviceLeasedMsg("an `adb` device command without -s can land on it", h)+
					"\n\nAnother phone: name its serial with `adb -s <serial> …`.", "")
			}
		}
	}
	return nil
}

func orTool(t string) string {
	if t == "" {
		return "an environment assignment"
	}
	return t
}

func deviceLeasedMsg(what string, h devlab.HeldLease) string {
	until := ""
	if !h.ExpiresAt.IsZero() {
		until = " until " + h.ExpiresAt.Local().Format(time.Kitchen)
	}
	return fmt.Sprintf(`%s: perflab has leased phone %s to %s%s.

A leased phone is driven only through perflab, the holder included: two
copies of one agent drove one phone on 2026-10-01 and reinstalled each
other's builds, so the night's numbers compared nothing.

  your lease:   perflab device shell %s --lease <token> -- <adb or devicectl args>
                (also: perflab device screencap|pull, perflab app launch)
  not yours:    perflab lease status %s --json`, what, h.DeviceID, h.Holder.String(), until, h.DeviceID, h.DeviceID)
}
