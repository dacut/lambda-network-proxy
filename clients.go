package main

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Client is a unique type for attaching clients to a context.
type Client int

const (
	ClientAWSLambda Client = iota
	ClientAWSS3
	ClientAWSSSM
)

type LambdaInvokeAPIClient interface {
	Invoke(ctx context.Context, input *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

type S3GetObjectAPIClient interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(s3.Options)) (*s3.GetObjectOutput, error)
}

type SSMGetParameterAPIClient interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(ssm.Options)) (*ssm.GetParameterOutput, error)
}
