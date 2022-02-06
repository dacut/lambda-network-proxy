package main

import (
	"fmt"
	"net"
	"strings"
)

func main() {
	netifs, err := net.Interfaces()
	if err != nil {
		panic(err)
	}

	for _, netif := range netifs {
		fmt.Printf("Interface %d: %s MTU %d MAC ", netif.Index, netif.Name, netif.MTU)

		for i, b := range netif.HardwareAddr {
			if i > 0 {
				fmt.Printf(":%02x", b)
			} else {
				fmt.Printf("%02x", b)
			}
		}

		var flags []string

		if netif.Flags&net.FlagUp != 0 {
			flags = append(flags, "UP")
		}

		if netif.Flags&net.FlagBroadcast != 0 {
			flags = append(flags, "BROADCAST")
		}

		if netif.Flags&net.FlagLoopback != 0 {
			flags = append(flags, "LOOPBACK")
		}

		if netif.Flags&net.FlagPointToPoint != 0 {
			flags = append(flags, "POINTTOPOINT")
		}

		if netif.Flags&net.FlagMulticast != 0 {
			flags = append(flags, "MULTICAST")
		}

		if len(flags) > 0 {
			fmt.Printf(" %s", strings.Join(flags, ","))
		}

		fmt.Printf("\n")

		netaddrs, err := netif.Addrs()
		if err != nil {
			fmt.Printf("Failed to get addresses for %s: %v\n", netif.Name, err)
		} else {
			for i, addr := range netaddrs {
				fmt.Printf("%2d %-5s %s\n", i, addr.Network(), addr.String())
			}
		}
	}
}
