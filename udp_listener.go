package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdaTypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	event "github.com/dacut/lambda-network-proxy-event-go"
)

type UDPListener struct {
	Config           *Config
	ListenerConfig   *ListenerConfig
	RemoteConnection *net.UDPConn
	Relays           map[string]*UDPRelay
	RelaysMutex      sync.Mutex
	Stopped          uint32
}

type UDPRelay struct {
	Listener           *UDPListener
	StartupPackets     []*UDPPacket
	ProxyConnection    *net.UDPConn
	RemoteAddress      *net.UDPAddr
	LambdaAddress      *net.UDPAddr
	Nonce              string
	StartupPacketMutex sync.Mutex
}

type UDPPacket struct {
	Message []byte
	OOB     []byte
}

func NewUDPListener(config *Config, listenerConfig *ListenerConfig) (*UDPListener, error) {
	if listenerConfig.Protocol != "udp" && listenerConfig.Protocol != "udp4" && listenerConfig.Protocol != "udp6" {
		return nil, fmt.Errorf("Invalid protocol: %s", listenerConfig.Protocol)
	}

	rc, err := net.ListenUDP(listenerConfig.Protocol, &net.UDPAddr{Port: int(listenerConfig.Port)})
	if err != nil {
		return nil, fmt.Errorf("Unable to listen on %s:%d: %w", listenerConfig.Protocol, listenerConfig.Port, err)
	}

	return &UDPListener{
		Config:           config,
		ListenerConfig:   listenerConfig,
		RemoteConnection: rc,
		Relays:           make(map[string]*UDPRelay),
		RelaysMutex:      sync.Mutex{},
		Stopped:          0,
	}, nil
}

func (ul *UDPListener) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		messageBuffer := make([]byte, 65536)
		oobBuffer := make([]byte, 65536)

		n, oobn, _, remoteAddr, err := ul.RemoteConnection.ReadMsgUDP(messageBuffer, oobBuffer)
		if err != nil {
			// Don't show the error message if we were gracefully stopped.
			if atomic.LoadUint32(&ul.Stopped) == 0 {
				log.Printf("Failed to accept connection: %s", err)
			}
			return
		}

		packet := &UDPPacket{
			Message: messageBuffer[:n],
			OOB:     oobBuffer[:oobn],
		}

		// See if we already have a relay for this connection.
		remoteAddrString := remoteAddr.String()
		ul.RelaysMutex.Lock()
		relay, found := ul.Relays[remoteAddrString]

		if !found {
			remoteAddr = CopyUDPAddr(remoteAddr)
			err := ul.StartUDPRelay(ctx, wg, packet, remoteAddr)

			if err != nil {
				log.Printf("Failed to create relay for %s: %v", remoteAddrString, err)
			}
			ul.RelaysMutex.Unlock()
		} else {
			ul.RelaysMutex.Unlock()
			relay.ReceivePacketFromRemote(packet)
		}
	}
}

func (ul *UDPListener) StartUDPRelay(ctx context.Context, wg *sync.WaitGroup, startPacket *UDPPacket, remoteAddr *net.UDPAddr) error {
	nonceBytes := make([]byte, 16)
	_, err := rand.Read(nonceBytes)
	if err != nil {
		log.Printf("Unable to create nonce: %s", err)
		return err
	}

	nonce := hex.EncodeToString(nonceBytes)

	remoteHost, remotePort, err := AddrToHostAndPort(remoteAddr.String())
	if err != nil {
		log.Printf("Unable to get remote host/port: %v", err)
		return err
	}

	proxyConnection, err := net.ListenUDP(ul.ListenerConfig.LambdaProtocol, nil)
	if err != nil {
		return err
	}

	proxyAddr := proxyConnection.LocalAddr()
	proxyHost, proxyPort, err := AddrToHostAndPort(proxyAddr.String())
	if err != nil {
		log.Printf("Unable to get Lambda proxy host/port: %v", err)
		proxyConnection.Close()
		return err
	}

	payload := event.ProxyEndpointEvent{
		ClientProtocol: remoteAddr.Network(),
		ClientAddress:  remoteHost,
		ClientPort:     remotePort,
		ProxyProtocol:  ul.ListenerConfig.LambdaProtocol,
		ProxyAddress:   proxyHost,
		ProxyPort:      proxyPort,
		Nonce:          nonce,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Unable to marshal payload: %v", err)
		proxyConnection.Close()
		return err
	}

	relay := &UDPRelay{
		Listener:           ul,
		StartupPackets:     []*UDPPacket{startPacket},
		ProxyConnection:    proxyConnection,
		RemoteAddress:      remoteAddr,
		Nonce:              nonce,
		StartupPacketMutex: sync.Mutex{},
	}

	// This function is called with the RelaysMutex held, so this is ok.
	ul.Relays[remoteAddr.String()] = relay

	// Invoke the Lambda function synchronously in a go routine; we exit the relay when it returns.
	wg.Add(1)
	go relay.InvokeAndWaitForLambda(ctx, wg, payloadBytes)

	// Start relaying packets from Lambda to the remote connection. We handle inbound packets differently.
	wg.Add(1)
	go relay.LambdaToRemoteLoop(ctx, wg)

	return nil
}

func (relay *UDPRelay) InvokeAndWaitForLambda(ctx context.Context, wg *sync.WaitGroup, payloadBytes []byte) {
	defer wg.Done()
	lambdaClient := ctx.Value(ClientAWSLambda).(LambdaInvokeAPIClient)
	ii := lambda.InvokeInput{
		FunctionName:   &relay.Listener.ListenerConfig.FunctionName,
		InvocationType: lambdaTypes.InvocationTypeRequestResponse,
		Payload:        payloadBytes,
	}
	_, err := lambdaClient.Invoke(ctx, &ii)

	// Lambda done. Stop relaying packets from the remote to Lambda.
	relay.Listener.RelaysMutex.Lock()
	delete(relay.Listener.Relays, relay.RemoteAddress.String())
	relay.Listener.RelaysMutex.Unlock()

	relay.ProxyConnection.Close()

	if err != nil {
		log.Printf("Lambda invocation on %s failed: %v", relay.Listener.ListenerConfig.FunctionName, err)
	}
}

func (relay *UDPRelay) LambdaToRemoteLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	messageBuffer := make([]byte, 65536)
	oobBuffer := make([]byte, 65536)
	var lambdaAddress *net.UDPAddr

	// Wait for the Nonce packet to be received.
	for {
		var n int
		var err error

		n, _, _, lambdaAddress, err = relay.ProxyConnection.ReadMsgUDP(messageBuffer, oobBuffer)
		if err != nil {
			log.Printf("Failed to receive packet from Lambda: %s", err)
			relay.ProxyConnection.Close()
			return
		}

		message := strings.TrimSpace(string(messageBuffer[:n]))
		if message == relay.Nonce {
			break
		}

		log.Printf("Invalid nonce recieved from %s: %s", lambdaAddress.String(), message)
	}

	// Send all outstanding packets to Lambda.
	relay.StartupPacketMutex.Lock()
	relay.LambdaAddress = lambdaAddress
	for _, packet := range relay.StartupPackets {
		_, _, err := relay.ProxyConnection.WriteMsgUDP(packet.Message, packet.OOB, relay.LambdaAddress)
		if err != nil {
			log.Printf("Failed to send packet to Lambda: %s", err)
			relay.ProxyConnection.Close()
			relay.StartupPacketMutex.Unlock()
			return
		}
	}
	relay.StartupPackets = nil
	relay.StartupPacketMutex.Unlock()

	// Relay packets from Lambda to the remote connection.
	for {
		var n, oobn int
		var err error

		n, oobn, _, lambdaAddress, err = relay.ProxyConnection.ReadMsgUDP(messageBuffer, oobBuffer)
		if err != nil {
			log.Printf("Failed to receive packet from Lambda: %s", err)
			relay.ProxyConnection.Close()
			return
		}

		if relay.LambdaAddress.String() != lambdaAddress.String() {
			log.Printf("Received a packet from unexpected endpoint %s: expected %s", lambdaAddress.String(), relay.LambdaAddress.String())
			continue
		}

		message := messageBuffer[:n]
		oob := oobBuffer[:oobn]

		_, _, err = relay.Listener.RemoteConnection.WriteMsgUDP(message, oob, relay.RemoteAddress)
		if err != nil {
			log.Printf("Failed to send packet to remote: %s", err)
			return
		}
	}
}

func (relay *UDPRelay) ReceivePacketFromRemote(packet *UDPPacket) {
	relay.StartupPacketMutex.Lock()
	if relay.LambdaAddress == nil {
		relay.StartupPackets = append(relay.StartupPackets, packet)
		relay.StartupPacketMutex.Unlock()
	} else {
		relay.StartupPacketMutex.Unlock()
		_, _, err := relay.ProxyConnection.WriteMsgUDP(packet.Message, packet.OOB, relay.LambdaAddress)
		if err != nil {
			log.Printf("Failed to send packet to Lambda: %s", err)
			relay.ProxyConnection.Close()
			return
		}
	}
}

func (ul *UDPListener) Stop() {
	atomic.StoreUint32(&ul.Stopped, 1)
	ul.RemoteConnection.Close()
}
