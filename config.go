package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

var validProtocols map[string]bool = map[string]bool{
	"tcp":  true,
	"tcp4": true,
	"tcp6": true,
	"udp":  true,
	"udp4": true,
	"udp6": true,
}

var validProtocolsString string

func init() {
	vpsBuilder := strings.Builder{}
	first := true
	for k := range validProtocols {
		if !first {
			vpsBuilder.WriteString(", ")
		} else {
			first = false
		}

		vpsBuilder.WriteString(`"`)
		vpsBuilder.WriteString(k)
		vpsBuilder.WriteString(`"`)
	}

	validProtocolsString = vpsBuilder.String()
}

// Config is the top-level JSON/YAML configuration for the proxy server.
type Config struct {
	// Listeners is an array of Listener directives. At least one must be present.
	Listeners []*ListenerConfig `json:"Listeners"`

	// FunctionName is the default function to use for requests that do not specify a function.
	FunctionName string `json:"FunctionName,omitempty"`
}

// ListenerConfig defines a listener for the proxy server.
type ListenerConfig struct {
	// Protocol is the protocol to listen on, either "tcp" or "udp".
	Protocol string `json:"Protocol"`

	// LambdaProtocol is the protocol to listen on for Lambda requests. This defaults to the value used for Protocol.
	//
	// If Protocol is "tcp", "tcp4", or "tcp6", then LambdaProtocol must be "tcp", "tcp4", or "tcp6".
	//
	// If Protocol is "udp", "udp4", or "udp6", then LambdaProtocol must be "udp", "udp4", or "udp6".
	LambdaProtocol string `json:"LambdaProtocol,omitempty"`

	// Port is the port to listen on for the specified protocol.
	Port uint `json:"Port"`

	// FunctionName is the function to invoke for requests received on this listener. If a function is not specified,
	// the default function is used.
	FunctionName string `json:"FunctionName,omitempty"`
}

func (c *Config) validate() error {
	errors := []error{}
	if len(c.Listeners) == 0 {
		errors = append(errors, fmt.Errorf("No listeners specified"))
	}

	protocolMap := make(map[string]map[uint]int)
	for i, listener := range c.Listeners {
		errors = append(errors, listener.validate(i, c.FunctionName, protocolMap)...)
	}

	switch len(errors) {
	case 0:
		return nil
	case 1:
		return errors[0]
	default:
		errorsRest := strings.Builder{}
		for _, err := range errors[1:] {
			errorsRest.WriteString(" ")
			errorsRest.WriteString(err.Error())
		}
		return fmt.Errorf("Multiple errors: %w%s", errors[0], errorsRest.String())
	}
}

func (l *ListenerConfig) validate(index int, defaultFunctionName string, protocolMap map[string]map[uint]int) []error {
	errors := []error{}

	if l.LambdaProtocol == "" {
		l.LambdaProtocol = l.Protocol
	} else {
		_, found := protocolMap[l.Protocol]
		if !found {
			errors = append(errors, fmt.Errorf(
				"Invalid Listeners[%d].LambdaProtocol value: %#v: expected one of: %s.", index, l.LambdaProtocol,
				validProtocolsString))
		} else if strings.HasPrefix(l.Protocol, "tcp") && !strings.HasPrefix(l.LambdaProtocol, "tcp") {
			errors = append(errors, fmt.Errorf(
				"Listeners[%d].LambdaProtocol value %#v must be a TCP protocol to match Listeners[%d].Protocol value %#v.",
				index, l.LambdaProtocol, index, l.Protocol))
		} else if strings.HasPrefix(l.Protocol, "udp") && !strings.HasPrefix(l.LambdaProtocol, "udp") {
			errors = append(errors, fmt.Errorf(
				"Listeners[%d].LambdaProtocol value %#v must be a UDP protocol to match Listeners[%d].Protocol value %#v.",
				index, l.LambdaProtocol, index, l.Protocol))
		}
	}

	if l.Port == 0 || l.Port > 65535 {
		errors = append(errors, fmt.Errorf("Invalid port %d on listener %d: must be between 1-65535.", l.Port, index))
	}

	portMap, found := protocolMap[l.Protocol]
	if !found {
		portMap = make(map[uint]int)
		protocolMap[l.Protocol] = portMap
	}

	existing, found := portMap[l.Port]
	if !found {
		portMap[l.Port] = index
	} else {
		errors = append(errors, fmt.Errorf(
			"Multiple listeners on port %d and protocol %s: %d and %d", l.Port, l.Protocol, existing, index))
	}

	if l.FunctionName == "" && defaultFunctionName == "" {
		errors = append(errors, fmt.Errorf(
			"No function specified on listener %d and no default function specified.", index))
	}

	return errors
}

func ConfigFromURL(ctx context.Context, configURLString string) (*Config, error) {
	configURL, err := url.Parse(configURLString)
	if err != nil {
		return nil, err
	}

	switch configURL.Scheme {
	case "file", "":
		return ConfigFromFile(ctx, configURL)
	case "s3":
		return ConfigFromS3(ctx, configURL)
	case "ssm":
		return ConfigFromSSM(ctx, configURL)
	default:
		return nil, fmt.Errorf("Unknown config URL scheme: %s", configURL.Scheme)
	}
}

func ConfigFromFile(ctx context.Context, configURL *url.URL) (*Config, error) {
	configData, err := os.ReadFile(configURL.Path)
	if err != nil {
		return nil, fmt.Errorf("Unable to open config file %s: %w", configURL.Path, err)
	}

	return ConfigFromBytes(configData)
}

func ConfigFromS3(ctx context.Context, configURL *url.URL) (*Config, error) {
	s3Client := ctx.Value(ClientAWSS3).(S3GetObjectAPIClient)

	goi := s3.GetObjectInput{
		Bucket: &configURL.Host,
		Key:    &configURL.Path,
	}

	goo, err := s3Client.GetObject(ctx, &goi)
	if err != nil {
		return nil, fmt.Errorf("Unable to get config from S3 URL %s: %w", configURL.String(), err)
	}
	defer goo.Body.Close()

	result, err := io.ReadAll(goo.Body)
	if err != nil {
		return nil, fmt.Errorf("Unable to read config from S3 URL %s: %w", configURL.String(), err)
	}

	return ConfigFromBytes(result)
}

func ConfigFromSSM(ctx context.Context, configURL *url.URL) (*Config, error) {
	ssmClient := ctx.Value(ClientAWSSSM).(SSMGetParameterAPIClient)
	gpi := ssm.GetParameterInput{
		Name:           &configURL.Path,
		WithDecryption: true,
	}
	gpo, err := ssmClient.GetParameter(ctx, &gpi)
	if err != nil {
		return nil, fmt.Errorf("Unable to get config from SSM parameter %s: %w", configURL.String(), err)
	}

	return ConfigFromBytes([]byte(*gpo.Parameter.Value))
}

func ConfigFromBytes(configData []byte) (*Config, error) {
	config := &Config{}
	err := json.Unmarshal(configData, config)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse config: %w", err)
	}

	err = config.validate()
	if err == nil {
		return config, nil
	}

	return nil, err
}
