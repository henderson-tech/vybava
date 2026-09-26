# vpn

`vybava vpn` (or the installed `vpn` applet) runs named WireGuard tunnels on
macOS outside WireGuard.app, as LaunchDaemons that start at boot and restart
when the interface dies. It exists because the lovinka-admin tunnel carries
10.8.1.1, the DNS server every `/etc/resolver` file for the admin domains
points at. When that tunnel is down, every lookup those files send there,
`app.vitrinka.ai` included, stalls until it times out.

```sh
vybava vpn add lovinka-admin --ref 'onyx://WireGuard/lovinka-admin%20Mac%20CLI%20profile/Configuration' \
  --probe 10.8.1.1:443 --dns 10.8.1.1
vybava vpn install lovinka-admin --dry-run   # print the sudo steps, run nothing
vybava vpn install lovinka-admin             # Onyx approval, then one sudo prompt
vybava vpn status [NAME...] [--json]         # exits 2 when down or the tunnel DNS is silent
vybava vpn uninstall lovinka-admin           # stop, remove daemon + supervisor + key
```

Prerequisites: Homebrew `bash`, `wireguard-tools` and `wireguard-go`, plus a
running, unlocked Onyx with its local MCP service.

## Registration (`add`)

`~/.config/vybava/vpn/<name>.json` holds the Onyx reference, TCP probes, the
tunnel's DNS server and optional `excludePeers` public keys. It never holds a
key. On an existing registration, `add` changes only the flags you pass.

## Install

1. Onyx injects the profile into `<resolved exe> vpn _apply <name> <dir> <fifo>`.
   The vault item's `allowed_commands` must permit that prefix, and the item's
   approval tier applies. The child rejects everything wg-quick would
   execute (hooks, `SaveConfig`, `Table`), drops the `excludePeers`, and
   writes the result into a private FIFO. It refuses to write to any regular
   file.
2. The CLI runs each privileged step as `sudo <argv>` from your terminal
   (`--dry-run` prints them). It writes three root-owned files:
   - `/usr/local/etc/vybava/wireguard/<name>.conf`: 0600, in a 0700
     root:wheel directory. This is the only place the key rests on disk. It
     has to be there because a boot-time daemon cannot reach Onyx.
   - `/usr/local/etc/vybava/wireguard/<name>.sh`: the supervisor.
   - `/Library/LaunchDaemons/com.vybava.vpn.<name>.plist`: `RunAtLoad`, plus
     `KeepAlive {SuccessfulExit: false}` with a 30 s throttle.
3. Any job that already holds the label is booted out first. That includes
   the transient `launchctl submit` recovery job from 2026-09-22. So running
   `install` again restarts the tunnel with the vault's current profile.

The supervisor clears any interface left behind by an earlier run and runs
`wg-quick up`. It then lives exactly as long as the interface's socket, so
launchd's job state is the tunnel's state. When launchd stops it (bootout or
uninstall), it runs `wg-quick down`. If the interface vanishes, or
`wg-quick up` fails before the network is ready at boot, the supervisor exits
1 and launchd retries it. Logs go to `/var/log/vybava-vpn/<name>.log`.

Trust boundary: the daemon runs Homebrew's `bash`/`wg-quick` as root, the same
trust `sudo wg-quick` already implies on a Homebrew Mac. Nothing it runs lives
under your home directory.

## Status: the route is the truth

WireGuard.app and `scutil --nc` only know about the app's own profiles, so
they report `Disconnected` for a tunnel that wg-quick carries. Status
therefore checks, per tunnel:

- the kernel route to the tunnel's DNS server (or first probe);
- whether that interface is wg-quick's (`/var/run/wireguard/<utun>.sock` beside
  `<name>.name`);
- the launchd job holding `com.vybava.vpn.<name>`:
  - `persistent`: our plist;
  - `transient`: a `submit` job that is gone at reboot;
  - `other` or `none`;
- the app's state;
- one DNS question to the tunnel's resolver (any reply counts, 2 s budget);
- the TCP probes.

A healthy line reads `up via wg-quick (utun11)`. The info diagnostic
`VPN_APP_UNAWARE` says out loud that the app's `Disconnected` is expected.

| Code | Severity | Meaning → next |
|---|---|---|
| `VPN_DOWN` | error | no tunnel carries it → `install`, `launchctl bootstrap` or the log |
| `VPN_DNS_SILENT` | error | the tunnel is up but its DNS does not answer → `kickstart -k` |
| `VPN_PROBE_FAILED` | warning | a probe target is unreachable through the tunnel |
| `VPN_NOT_PERSISTENT` | warning | wg-quick carries it, but not our LaunchDaemon → `install` |
| `VPN_DUPLICATE` | warning | the app also has the same identity connected |
| `VPN_APP_UNAWARE` | info | the app reports Disconnected while wg-quick carries it |

`--json` emits the runx envelope (`data` = one status per tunnel); every fix
also appears in `next`.
