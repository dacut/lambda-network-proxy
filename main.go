package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

func main() {
	help := flag.Bool("help", false, "Show this usage information.")
	configURL := flag.String("config", "", strings.TrimSpace(`
The URL to the configuration file. This can be a SSM parameter (ssm://), S3
object (s3://), or a local file (file://).`))
	flag.Parse()

	if help != nil && *help {
		ShowUsage(os.Stdout)
		os.Exit(0)
	}

	if configURL == nil || *configURL == "" {
		log.Printf("No configuration URL provided")
		ShowUsage(os.Stderr)
		os.Exit(1)
	}

	ctx := context.Background()

	awsCfg, err := awsConfig.LoadDefaultConfig(ctx)
	if err != nil {
		panic(fmt.Errorf("Unable to load AWS config: %w", err))
	}
	lambdaClient := lambda.NewFromConfig(awsCfg)
	s3Client := s3.NewFromConfig(awsCfg)
	var ssmClient SSMGetParameterAPIClient = ssm.NewFromConfig(awsCfg)

	ctx = context.WithValue(ctx, ClientAWSLambda, lambdaClient)
	ctx = context.WithValue(ctx, ClientAWSS3, s3Client)
	ctx = context.WithValue(ctx, ClientAWSSSM, ssmClient)

	config, err := ConfigFromURL(ctx, *configURL)
	if err != nil {
		panic(err)
	}

	haltChannel := make(chan os.Signal, 1)
	signal.Notify(haltChannel, syscall.SIGTERM, syscall.SIGINT)
	wg := &sync.WaitGroup{}

	listeners := make([]Listener, 0, len(config.Listeners))

	for _, listenerConfig := range config.Listeners {
		if listenerConfig.FunctionName == "" {
			listenerConfig.FunctionName = config.FunctionName
		}

		var listener Listener
		var err error

		if strings.HasPrefix(listenerConfig.Protocol, "tcp") {
			listener, err = NewTCPListener(config, listenerConfig)
		} else {
			listener, err = NewUDPListener(config, listenerConfig)
		}

		if err != nil {
			log.Printf("Failed to create listener for %s port %d: %v", listenerConfig.Protocol, listenerConfig.Port, err)
			os.Exit(1)
		}

		listeners = append(listeners, listener)
	}

	for _, listener := range listeners {
		wg.Add(1)
		go listener.Run(ctx, wg)
	}

	sigNumber := <-haltChannel
	log.Printf("Received signal %s; shutting down", sigNumber)

	for _, listener := range listeners {
		listener.Stop()
	}

	wg.Wait()
}

func ShowUsage(w io.Writer) {
	fmt.Fprintf(w, "Proxy between a network load balancer and a Lambda function.\n")
	fmt.Fprintf(w, "Usage: %s [options]\n", os.Args[0])
	flag.CommandLine.SetOutput(w)
	flag.PrintDefaults()
	os.Exit(1)
}
