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
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"os"
)

func main() {
	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	l := &layerLister{
		dynamodb: dynamodb.NewFromConfig(cfg),
		table:    os.Getenv("TABLE"),
	}

	lambda.Start(l.handle)
}

type layerListerInput struct {
	Repo   string `json:"Repo"`
	Digest string `json:"Digest"`
}

type layerListerOutput struct {
	Layers []string
}

type layerLister struct {
	dynamodb *dynamodb.Client
	table    string
}

func (ll *layerLister) handle(ctx context.Context, input *layerListerInput) (any, error) {
	refstr := fmt.Sprintf("%s@%s", input.Repo, input.Digest)
	ref, err := name.ParseReference(refstr)
	if err != nil {
		return nil, fmt.Errorf("parsing ref: %w", err)
	}

	img, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, fmt.Errorf("getting image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting layers: %w", err)
	}

	digests := []string{}
	var totalSize int64

	for _, layer := range layers {
		size, _ := layer.Size()
		totalSize += size

		digest, err := layer.Digest()
		if err != nil {
			return nil, fmt.Errorf("getting layer digest: %w", err)
		}

		digests = append(digests, digest.String())
	}

	key := &bitypes.ImageInfoKey{
		Repo:   input.Repo,
		Digest: input.Digest,
	}

	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return nil, fmt.Errorf("getting raw config: %w", err)
	}

	manifest, err := img.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("getting manifest: %w", err)
	}

	_, err = ll.dynamodb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        &ll.table,
		Key:              key.Key(),
		UpdateExpression: aws.String("SET TotalSize = :TotalSize, RawConfig = :RawConfig, Manifest = :Manifest"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":TotalSize": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", totalSize)},
			":RawConfig": &types.AttributeValueMemberB{Value: rawConfig},
			":Manifest":  &types.AttributeValueMemberB{Value: manifest},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("updating image data in dynamo: %w", err)
	}

	return &layerListerOutput{
		Layers: digests,
	}, nil
}
