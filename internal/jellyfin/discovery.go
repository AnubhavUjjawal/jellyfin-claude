package jellyfin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"
)

// DiscoveryPort is the UDP port Jellyfin listens on for LAN auto-discovery.
const DiscoveryPort = 7359

const discoveryMessage = "Who is JellyfinServer?"

// DiscoveredServer is a Jellyfin server's reply to a discovery probe.
type DiscoveredServer struct {
	Address         string `json:"Address"`
	ID              string `json:"Id"`
	Name            string `json:"Name"`
	EndpointAddress string `json:"EndpointAddress,omitempty"`
}

// Discover broadcasts Jellyfin's discovery probe on every IPv4 interface (plus any
// extra unicast targets) and collects the replies until timeout.
func Discover(ctx context.Context, timeout time.Duration, extra ...string) ([]DiscoveredServer, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	targets := broadcastAddrs()
	for _, host := range extra {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil {
			return nil, err
		}
		targets = append(targets, ips...)
	}

	var sent int
	var sendErr error
	for _, ip := range targets {
		if _, err := conn.WriteToUDP([]byte(discoveryMessage), &net.UDPAddr{IP: ip, Port: DiscoveryPort}); err != nil {
			sendErr = err
			continue
		}
		sent++
	}
	if sent == 0 {
		if sendErr == nil {
			sendErr = errors.New("no IPv4 network interfaces to broadcast on")
		}
		return nil, sendErr
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	seen := map[string]bool{}
	var found []DiscoveredServer
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return found, nil
			}
			return found, err
		}
		var s DiscoveredServer
		if json.Unmarshal(buf[:n], &s) != nil || s.Address == "" {
			continue
		}
		key := s.ID
		if key == "" {
			key = s.Address
		}
		if !seen[key] {
			seen[key] = true
			found = append(found, s)
		}
	}
}

// broadcastAddrs returns the limited broadcast address plus the directed broadcast
// address of each up, non-loopback IPv4 interface. Some OSes only route the
// directed form, so both are sent.
func broadcastAddrs() []net.IP {
	out := []net.IP{net.IPv4bcast}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || len(ipn.Mask) != net.IPv4len {
				continue
			}
			bc := make(net.IP, net.IPv4len)
			for i := range ip4 {
				bc[i] = ip4[i] | ^ipn.Mask[i]
			}
			out = append(out, bc)
		}
	}
	return out
}
