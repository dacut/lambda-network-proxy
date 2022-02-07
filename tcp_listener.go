package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdaTypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	event "github.com/dacut/lambda-network-proxy-event-go"
)

type TCPListener struct {
	Config         *Config
	ListenerConfig *ListenerConfig
	RemoteListener *net.TCPListener
	Stopped        uint32
}

type TCPRelay struct {
	Listener         *TCPListener
	RemoteConnection *net.TCPConn
	RemoteAddress    net.Addr
	LambdaListener   *net.TCPListener
	LambdaConnection *net.TCPConn
	LambdaAddress    net.Addr
	Nonce            string
	StoppedAccepting uint32
	StoppedRelaying  uint32
}

func NewTCPListener(config *Config, listenerConfig *ListenerConfig) (*TCPListener, error) {
	if listenerConfig.Protocol != "tcp" && listenerConfig.Protocol != "tcp4" && listenerConfig.Protocol != "tcp6" {
		return nil, fmt.Errorf("Invalid protocol: %s", listenerConfig.Protocol)
	}

	rl, err := net.ListenTCP(listenerConfig.Protocol, &net.TCPAddr{Port: int(listenerConfig.Port)})
	if err != nil {
		return nil, fmt.Errorf("Unable to listen on %s:%d: %w", listenerConfig.Protocol, listenerConfig.Port, err)
	}

	return &TCPListener{
		Config:         config,
		ListenerConfig: listenerConfig,
		RemoteListener: rl,
		Stopped:        0,
	}, nil
}

func (tl *TCPListener) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		// Wait for an incomming connection request.
		remoteConn, err := tl.RemoteListener.AcceptTCP()
		if err != nil {
			// Don't show the error message if we were gracefully stopped.
			if atomic.LoadUint32(&tl.Stopped) == 0 {
				log.Printf("Failed to accept connection: %s", err)
			}
			return
		}

		// Connection request received. Spawn off a connection handler.
		tl.StartTCPRelay(ctx, wg, remoteConn)
	}
}

func (tl *TCPListener) StartTCPRelay(ctx context.Context, wg *sync.WaitGroup, remoteConn *net.TCPConn) error {
	remoteConn.SetNoDelay(true)
	remoteAddr := remoteConn.RemoteAddr()
	ipFamily := FamilyFromProtocol(tl.ListenerConfig.LambdaProtocol)
	addrs, err := GetLocalAddresses(ipFamily)
	if err != nil {
		log.Printf("Unable to get available local addresses: %v", err)
		return err
	}

	nonceBytes := make([]byte, 16)
	_, err = rand.Read(nonceBytes)
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

	proxyAddr := &net.TCPAddr{
		IP:   addrs[0],
		Port: 0,
	}

	lambdaListener, err := net.ListenTCP(tl.ListenerConfig.LambdaProtocol, proxyAddr)
	if err != nil {
		return err
	}

	// Get the port assigned to us.
	proxyHost, proxyPort, err := AddrToHostAndPort(lambdaListener.Addr().String())
	if err != nil {
		log.Printf("Unable to get Lambda proxy host/port: %v", err)
		lambdaListener.Close()
		return err
	}

	payload := event.ProxyEndpointEvent{
		ClientProtocol: remoteAddr.Network(),
		ClientAddress:  remoteHost,
		ClientPort:     remotePort,
		ProxyProtocol:  tl.ListenerConfig.LambdaProtocol,
		ProxyAddress:   proxyHost,
		ProxyPort:      proxyPort,
		Nonce:          nonce,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Unable to marshal payload: %v", err)
		lambdaListener.Close()
		return err
	}

	relay := &TCPRelay{
		Listener:         tl,
		RemoteConnection: remoteConn,
		RemoteAddress:    remoteAddr,
		LambdaListener:   lambdaListener,
		LambdaConnection: nil,
		LambdaAddress:    nil,
		Nonce:            nonce,
		StoppedAccepting: 0,
		StoppedRelaying:  0,
	}

	// Invoke the Lambda function synchronously in a go routine; we exit the relay when it returns.
	wg.Add(1)
	go relay.InvokeAndWaitForLambda(ctx, wg, payloadBytes)

	// Accept the connection from Lambda, then relay packets in the background.
	wg.Add(1)
	go relay.AcceptAndRelay(ctx, wg)

	return nil
}

func (relay *TCPRelay) InvokeAndWaitForLambda(ctx context.Context, wg *sync.WaitGroup, payloadBytes []byte) {
	defer wg.Done()
	payloadString := string(payloadBytes)
	lambdaClient := ctx.Value(ClientAWSLambda).(LambdaInvokeAPIClient)
	ii := lambda.InvokeInput{
		FunctionName:   &relay.Listener.ListenerConfig.FunctionName,
		InvocationType: lambdaTypes.InvocationTypeRequestResponse,
		Payload:        payloadBytes,
	}

	log.Printf("Invoking Lambda function %s with payload %s", relay.Listener.ListenerConfig.FunctionName, payloadString)
	result, err := lambdaClient.Invoke(ctx, &ii)
	if err != nil {
		log.Printf("Lambda invocation for payload %s failed: %v", payloadString, err)
	} else {
		if result == nil {
			log.Printf("Lambda invocation for payload %s succeeded without a result", payloadString)
		} else if result.FunctionError != nil && *result.FunctionError != "" {
			log.Printf("Lambda invocation for payload %s failed: %s", payloadString, *result.FunctionError)
		} else {
			log.Printf("Lambda invocation for payload %s succeeded: %s", payloadString, string(result.Payload))
		}
	}

	if result != nil && result.LogResult != nil && *result.LogResult != "" {
		d := base64.NewDecoder(base64.StdEncoding, strings.NewReader(*result.LogResult))
		result, err := io.ReadAll(d)
		if err == nil {
			log.Printf("%s", string(result))
		}
	}

	// Lambda done. Stop relaying packets from the remote to Lambda.
	atomic.StoreUint32(&relay.StoppedAccepting, 1)
	atomic.StoreUint32(&relay.StoppedRelaying, 1)

	if relay.LambdaListener != nil {
		relay.LambdaListener.Close()
	}

	if relay.LambdaConnection != nil {
		relay.LambdaConnection.Close()
	}

	if relay.RemoteConnection != nil {
		relay.RemoteConnection.Close()
	}

	if err != nil {
		log.Printf("Lambda invocation on %s failed: %v", relay.Listener.ListenerConfig.FunctionName, err)
	}
}

func (relay *TCPRelay) AcceptAndRelay(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer relay.LambdaListener.Close()

	lambdaAddress := relay.LambdaListener.Addr().String()

	// Wait for the connection to be accepted and the nonce to be received.
	for {
		lambdaConn, err := relay.LambdaListener.AcceptTCP()
		if err != nil {
			if atomic.LoadUint32(&relay.StoppedAccepting) == 0 {
				log.Printf("Failed to accept Lambda connection on %s: %v", lambdaAddress, err)
			}

			return
		}

		// We run this in a goroutine to prevent someone from connecting and holding onto the connection indefinitely,
		// causing a denial-of-service
		wg.Add(1)
		go relay.GetNonceAndRelay(ctx, wg, lambdaConn)
	}
}

func (relay *TCPRelay) GetNonceAndRelay(ctx context.Context, wg *sync.WaitGroup, lambdaConn *net.TCPConn) {
	defer wg.Done()
	lambdaConn.SetNoDelay(true)
	lambdaAddr := lambdaConn.RemoteAddr().String()

	readBuffer := make([]byte, 65536)

	// The Lambda has 5 seconds to send us the nonce
	lambdaConn.SetReadDeadline(time.Now().Add(5 * time.Second))

	log.Printf("Reading nonce from %s", lambdaAddr)
	nonceData := make([]byte, 0, len(relay.Nonce))
	var dataBeyondNonce []byte

	// Loop until we have enough nonce data.
	for {
		n, err := lambdaConn.Read(readBuffer)
		if err != nil {
			var opError *net.OpError

			// Ignore timeouts
			if isOpError := errors.As(err, &opError); isOpError && opError.Timeout() {
				log.Printf("Connection from %s timed out without receiving a nonce", lambdaAddr)
			} else {
				log.Printf("Reading nonce from %s failed: %v", lambdaAddr, err)
			}

			lambdaConn.Close()
			return
		}

		log.Printf("Got %d bytes while waiting for nonce", n)

		nonceNeeded := len(relay.Nonce) - len(nonceData)
		nonceToAdd := nonceNeeded
		if nonceToAdd > n {
			nonceToAdd = n
		}

		nonceData = append(nonceData, readBuffer[:nonceToAdd]...)
		if len(nonceData) == len(relay.Nonce) {
			dataBeyondNonce = readBuffer[nonceToAdd:n]
			break
		}
	}

	// No longer need a deadline
	lambdaConn.SetReadDeadline(time.Time{})

	// Verify the nonce
	nonceString := string(nonceData)
	if nonceString != relay.Nonce {
		log.Printf("Invalid nonce recieved from %s", lambdaAddr)
		lambdaConn.Close()
		return
	}

	// Nonce is correct. Stop accepting connections.
	atomic.StoreUint32(&relay.StoppedAccepting, 1)
	relay.LambdaListener.Close()

	relay.LambdaConnection = lambdaConn

	// If we received more data, send it to the remote.
	WriteBytes(relay.LambdaConnection, dataBeyondNonce)

	wg.Add(1)
	go relay.Relay(ctx, wg, relay.RemoteConnection, relay.LambdaConnection)

	wg.Add(1)
	go relay.Relay(ctx, wg, relay.LambdaConnection, relay.RemoteConnection)
}

func (relay *TCPRelay) Relay(ctx context.Context, wg *sync.WaitGroup, dst, src *net.TCPConn) {
	defer wg.Done()
	defer dst.CloseWrite()
	defer src.CloseRead()

	srcAddr := src.RemoteAddr().String()
	dstAddr := dst.RemoteAddr().String()

	log.Printf("Relaying from %s to %s", srcAddr, dstAddr)
	buffer := make([]byte, 65536)
	nWritten := 0

	for {
		n, err := src.Read(buffer)
		if err != nil {
			stopped := atomic.LoadUint32(&relay.StoppedRelaying)
			if stopped == 0 && !errors.Is(err, io.EOF) {
				log.Printf("Relaying from %s to %s failed: %v", srcAddr, dstAddr, err)
			} else {
				log.Printf("Connection from %s to %s closed", srcAddr, dstAddr)
			}
			break
		}

		if n == 0 {
			log.Printf("Connection from %s to %s closed", srcAddr, dstAddr)
			break
		}

		_, err = WriteBytes(dst, buffer[:n])
		if err != nil {
			stopped := atomic.LoadUint32(&relay.StoppedRelaying)
			if stopped == 0 {
				log.Printf("Relaying from %s to %s failed: %v", srcAddr, dstAddr, err)
			}
			break
		}

		nWritten += n
	}

	log.Printf("Relayed %d bytes from %v to %v", nWritten, srcAddr, dstAddr)
}

func (tl *TCPListener) Stop() {
	atomic.StoreUint32(&tl.Stopped, 1)
	tl.RemoteListener.Close()
}
