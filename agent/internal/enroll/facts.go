// Package enroll implements W1-4 — the agent's self-enrollment against the
// server's AgentService.
//
// Lifecycle:
//  1. First boot with a bootstrap token: Gather() host facts, Enroll() them
//     in exchange for a persistent identity (device_id + agent JWT), then
//     Save() that identity to disk (0600).
//  2. Every later boot: Load() the persisted identity and REUSE it — a
//     restarted agent never calls Enroll again (re-enroll would mint a second
//     identity / is refused by the server, and would defeat the whole
//     "stable device_id" model).
//
// The bootstrap token is a one-time code the operator embedded in the
// one-line bootstrap installer (W1-3). After the first successful enroll it
// is consumed: the agent strips it from its --config file itself (L3), and
// the server's grace window (ingest.BootstrapGraceWindow) covers the window
// between a successful enroll and a failed persist — replaying the same
// token rebinds the same device instead of erroring (H2).
package enroll

import (
	"fmt"
	"net"
	"os"
	"runtime"
)

// Facts are the host facts reported to the server during Enroll, so it can
// create the device row before the first heartbeat.
type Facts struct {
	Hostname     string
	OS           string   // "linux" | "darwin" | "windows"
	Arch         string   // "amd64" | "arm64" | ...
	AgentVersion string   // stamped at build time (main.version)
	Interfaces   []string // "eth0:10.0.0.5/24" style
}

// Gather collects host facts for enrollment. It never fails hard: a missing
// hostname or interface list degrades to an empty value rather than blocking
// enrollment (the server fills in what it can; richer inventory is W1-7).
func Gather(agentVersion string) Facts {
	hostname, _ := os.Hostname()

	ifaces := make([]string, 0)
	if list, err := net.Interfaces(); err == nil {
		for _, i := range list {
			if i.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := i.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				// IPv4 only in v1 — IPv6/CIDR inventory arrives with W1-7.
				if ipn.IP.To4() == nil {
					continue
				}
				ifaces = append(ifaces, fmt.Sprintf("%s:%s", i.Name, ipn.String()))
			}
		}
	}

	return Facts{
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		AgentVersion: agentVersion,
		Interfaces:   ifaces,
	}
}
