package main

import (
	"fmt"
	"strconv"
	"strings"
)

func AddrToHostAndPort(addr string) (string, uint, error) {
	sep := strings.LastIndex(addr, ":")

	if sep <= 0 {
		return "", 0, fmt.Errorf("Unable to split address into host and port: %#v", addr)
	}

	host := addr[:sep]
	if host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}

	port, err := strconv.ParseUint(addr[sep+1:], 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("Unable to split address into host and port: %#v", addr)
	}

	if port == 0 || port > 65535 {
		return "", 0, fmt.Errorf("Unable to split address into host and port: %#v", addr)
	}

	return host, uint(port), nil
}
