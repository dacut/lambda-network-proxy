package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
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
		conn, err := tl.RemoteListener.AcceptTCP()
		if err != nil {
			// Don't show the error message if we were gracefully stopped.
			if atomic.LoadUint32(&tl.Stopped) == 0 {
				log.Printf("Failed to accept connection: %s", err)
			}
			return
		}

		wg.Add(1)
		go tl.HandleConnection(ctx, wg, conn)
	}
}

func (tl *TCPListener) HandleConnection(ctx context.Context, wg *sync.WaitGroup, conn *net.TCPConn) {
	defer wg.Done()
	defer conn.Close()

	lambdaClient := ctx.Value(ClientAWSLambda).(LambdaInvokeAPIClient)

	remoteAddr := conn.RemoteAddr()
	remoteHost, remotePort, err := AddrToHostAndPort(remoteAddr.String())
	if err != nil {
		log.Printf("Unable to get remote host/port: %v", err)
		conn.Close()
		return
	}

	listenContext, listenComplete := context.WithTimeout(ctx, 10*time.Second)
	defer listenComplete()

	listenConfig := net.ListenConfig{}
	l, err := listenConfig.Listen(listenContext, tl.ListenerConfig.LambdaProtocol, "")
	if err != nil {
		log.Printf("Failed to create a Lambda lister on %s: %s", tl.ListenerConfig.LambdaProtocol, err)
		conn.Close()
		return
	}

	lambdaAddr := l.Addr()
	lambdaHost, lambdaPort, err := AddrToHostAndPort(lambdaAddr.String())
	if err != nil {
		log.Printf("Unable to get Lambda proxy host/port: %v", err)
		l.Close()
		conn.Close()
		return
	}

	payload := event.ProxyEndpointEvent{
		ClientProtocol: remoteAddr.Network(),
		ClientAddress:  remoteHost,
		ClientPort:     remotePort,
		ProxyProtocol:  tl.ListenerConfig.LambdaProtocol,
		ProxyAddress:   lambdaHost,
		ProxyPort:      lambdaPort,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Unable to marshal payload: %v", err)
		conn.Close()
		l.Close()
		return
	}

	ii := lambda.InvokeInput{
		FunctionName:   &tl.ListenerConfig.FunctionName,
		InvocationType: lambdaTypes.InvocationTypeEvent,
		Payload:        payloadBytes,
	}
	_, err = lambdaClient.Invoke(ctx, &ii)
	if err != nil {
		log.Printf("Lambda invocation on %s failed: %v", tl.ListenerConfig.FunctionName, err)
		conn.Close()
		l.Close()
		return
	}

	lambdaConnGeneric, err := l.Accept()
	defer l.Close()
	listenComplete()
	if err != nil {
		select {
		case <-ctx.Done():
			log.Printf("Stopping listener %s:%d\n", tl.ListenerConfig.LambdaProtocol, lambdaPort)
			return

		default:
			log.Printf("Failed to accept connection: %s", err)
			conn.Close()
		}
	}

	lambdaConn := lambdaConnGeneric.(*net.TCPConn)
	l.Close()

	wg.Add(2)
	go Relay(ctx, wg, conn, lambdaConn)
	go Relay(ctx, wg, lambdaConn, conn)
}

func (tl *TCPListener) Stop() {
	atomic.StoreUint32(&tl.Stopped, 1)
	tl.RemoteListener.Close()
}
