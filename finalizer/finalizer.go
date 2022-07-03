package main

import (
	"browseimage/bitypes"
	"context"
	"fmt"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"os"
	"time"
)

type finalizer struct {
	dynamo *dynamodb.Client
	table  string
}

type FinalizerInput struct {
	Payload struct {
		Repo   string
		Digest string
	}
	Meta struct {
		ExecutionId string
		StartTime   time.Time
		Error       any `json:",omitempty"`
	}
}

type FinalizerOutput struct {
}

func (f *finalizer) handle(ctx context.Context, input *FinalizerInput) (*FinalizerOutput, error) {
	key := &bitypes.ImageInfoKey{
		Repo:   input.Payload.Repo,
		Digest: input.Payload.Digest,
	}

	status := "SUCCEEDED"
	if input.Meta.Error != nil {
		status = "FAILED"
	}

	duration := time.Now().Sub(input.Meta.StartTime)

	_, err := f.dynamo.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        &f.table,
		Key:              key.Key(),
		UpdateExpression: aws.String("SET #status = :status, #duration = :duration, ExecutionId = :executionId"),
		//ConditionExpression: aws.String("ExecutionId = :executionId"),
		ExpressionAttributeNames: map[string]string{
			"#status":   "Status",
			"#duration": "Duration",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status":      &types.AttributeValueMemberS{Value: status},
			":duration":    &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", duration)},
			":executionId": &types.AttributeValueMemberS{Value: input.Meta.ExecutionId},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("updating item: %w", err)
	}

	return &FinalizerOutput{}, nil
}

func main() {
	ctx := context.Background()

	opts := []func(*config.LoadOptions) error{
		config.WithClientLogMode(aws.LogRequestWithBody | aws.LogResponseWithBody),
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		panic(err)
	}

	f := &finalizer{
		dynamo: dynamodb.NewFromConfig(cfg),
		table:  os.Getenv("TABLE"),
	}

	lambda.Start(f.handle)
}
