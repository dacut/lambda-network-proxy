package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
)

type IPFamily int

const (
	IPFamilyUnknown = iota
	IPv4
	IPv6
)

func AddrToHostAndPort(addr string) (string, uint, error) {
	host, portString, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}

	port, err := strconv.ParseUint(portString, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("Unable to parse port value %#v from address %#v: %w", portString, addr, err)
	}

	if port == 0 || port > 65535 {
		return "", 0, fmt.Errorf("Invalid port value %d from address %#v", port, addr)
	}

	return host, uint(port), nil
}

func CopyUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	ip := make(net.IP, len(addr.IP))
	copy(ip, addr.IP)
	return &net.UDPAddr{IP: ip, Port: addr.Port, Zone: addr.Zone}
}

func FamilyFromProtocol(protocol string) IPFamily {
	if protocol == "tcp4" || protocol == "udp4" {
		return IPv4
	} else if protocol == "tcp6" || protocol == "udp6" {
		return IPv6
	}

	return IPFamilyUnknown
}

func GetLocalAddresses(ipFamily IPFamily) ([]net.IP, error) {
	netifs, err := net.Interfaces()
	if err != nil {
		log.Printf("Unable to retrieve host interfaces: %v", err)
		return nil, err
	}

	var addresses []net.IP

	for _, netif := range netifs {
		if netif.Flags&net.FlagLoopback != 0 {
			continue
		}

		if netif.Flags&net.FlagUp == 0 {
			continue
		}

		ifaddrs, err := netif.Addrs()
		if err != nil {
			log.Printf("Failed to get addresses for interface %s: %v", netif.Name, err)
		} else {
			for _, ifaddr := range ifaddrs {
				ip, _, err := net.ParseCIDR(ifaddr.String())
				if err != nil {
					log.Printf("Unable to parse interface %s address %s: %v", netif.Name, ifaddr.String(), err)
				} else {
					if !ip.IsInterfaceLocalMulticast() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() &&
						!ip.IsMulticast() && !ip.IsUnspecified() && IPMatchesFamily(ip, ipFamily) {
						addresses = append(addresses, ip)
					}
				}
			}

		}
	}

	if len(addresses) > 0 {
		return addresses, nil
	}

	return nil, errors.New("No non-loopback interfaces attached and up")
}

func IPMatchesFamily(ip net.IP, ipFamily IPFamily) bool {
	if ipFamily == IPFamilyUnknown {
		return true
	}

	to4 := ip.To4()

	if ipFamily == IPv4 {
		return to4 != nil
	}

	return to4 == nil
}

func WriteBytes(w io.Writer, b []byte) (int, error) {
	totalWritten := 0
	for totalWritten < len(b) {
		n, err := w.Write(b[totalWritten:])
		if err != nil {
			return totalWritten, err
		}

		totalWritten += n
	}

	return totalWritten, nil
}
