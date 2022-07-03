package main

import (
	"browseimage/bitypes"
	"browseimage/layerreader"
	"browseimage/targzi"
	"context"
	"fmt"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"io"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithClientLogMode(aws.LogRequest|aws.LogResponse))
	if err != nil {
		panic(fmt.Sprintf("err %+v", err))
	}

	d := downloader{
		uploader: manager.NewUploader(s3.NewFromConfig(cfg)),
		bucket:   os.Getenv("BUCKET"),
		dynamodb: dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
			o.Retryer = retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 10
			})
		}),
		table: os.Getenv("TABLE"),
	}

	lambda.Start(d.handle)
}

type downloader struct {
	uploader *manager.Uploader
	bucket   string
	dynamodb *dynamodb.Client
	table    string
}

type transport struct{}

func (t transport) RoundTrip(request *http.Request) (*http.Response, error) {
	dump, _ := httputil.DumpRequestOut(request, false)
	fmt.Println(string(dump))

	resp, err := http.DefaultTransport.RoundTrip(request)
	if resp != nil {
		dump, _ = httputil.DumpResponse(resp, false)
		fmt.Println(string(dump))
	}

	return resp, err
}

func (d *downloader) handle(ctx context.Context, input *layerreader.RemoteInput) (*layerreader.RemoteOutput, error) {
	ref, err := name.ParseReference(fmt.Sprintf("%s@%s", input.Key.Repo, input.Key.Digest))
	if err != nil {
		return nil, fmt.Errorf("parsing ref: %w", err)
	}

	img, err := remote.Image(ref, remote.WithTransport(transport{}))
	if err != nil {
		return nil, fmt.Errorf("getting image for ref: %w", err)
	}

	layer, err := img.LayerByDigest(input.Layer)
	if err != nil {
		return nil, fmt.Errorf("getting remote layer: %w", err)
	}

	totalSize, err := layer.Size()
	if err != nil {
		return nil, fmt.Errorf("calculating layer total size: %w", err)
	}

	key := bitypes.LayerProgressKey{
		Repo:        input.Key.Repo,
		ImageDigest: input.Key.Digest,
		LayerDigest: input.Layer.String(),
	}

	_, err = d.dynamodb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &d.table,
		Item: (&bitypes.LayerProgress{
			LayerProgressKey: key,
			TotalBytes:       totalSize,
		}).Marshal(),
	})
	if err != nil {
		return nil, fmt.Errorf("putting initial layer progress metrics: %w", err)
	}

	raw, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("getting layer reader: %w", err)
	}

	counter := &CountReader{Reader: raw}
	var fileCount int64 = 0

	countctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go d.countProgress(countctx, &counter.count, &fileCount, key)

	dir, err := os.MkdirTemp("/tmp/", "targzi*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	index, err := targzi.BuildIndex(dir, counter, &fileCount)
	if err != nil {
		return nil, fmt.Errorf("building index: %w", err)
	}

	// TODO: there's a race condition here, but this should usually flush
	// the "downloaded" progress to dynamodb before this lambda function returns
	cancel()

	prefix := fmt.Sprintf("layers/%s/", input.Layer)

	gzf, err := os.Open(index.GzIndexPath)
	if err != nil {
		return nil, fmt.Errorf(": %w", err)
	}
	defer gzf.Close()
	gzfstat, _ := gzf.Stat()

	gziPut, err := d.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &d.bucket,
		Key:    aws.String(prefix + filepath.Base(index.GzIndexPath)),
		Body:   gzf,
	})
	if err != nil {
		return nil, fmt.Errorf(": %w", err)
	}

	ff, err := os.Open(index.FileIndexPath)
	if err != nil {
		return nil, fmt.Errorf(": %w", err)
	}
	defer ff.Close()
	ffstat, _ := ff.Stat()

	tarPut, err := d.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &d.bucket,
		Key:    aws.String(prefix + filepath.Base(index.FileIndexPath)),
		Body:   ff,
	})
	if err != nil {
		return nil, fmt.Errorf(": %w", err)
	}

	return &layerreader.RemoteOutput{
		Gzi: layerreader.Put{
			Key:       *gziPut.Key,
			VersionId: *gziPut.VersionID,
			Size:      gzfstat.Size(),
		},
		Tar: layerreader.Put{
			Key:       *tarPut.Key,
			VersionId: *tarPut.VersionID,
			Size:      ffstat.Size(),
		},
	}, nil
}

func (d *downloader) countProgress(ctx context.Context, byteCounter, fileCounter *int64, key bitypes.LayerProgressKey) {
	update := func() {
		byteCount := atomic.LoadInt64(byteCounter)
		fileCount := atomic.LoadInt64(fileCounter)

		_, err := d.dynamodb.UpdateItem(context.TODO(), &dynamodb.UpdateItemInput{
			TableName:        &d.table,
			Key:              key.Key(),
			UpdateExpression: aws.String("SET CompletedBytes = :CompletedBytes, CompletedFiles = :CompletedFiles"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":CompletedBytes": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", byteCount)},
				":CompletedFiles": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", fileCount)},
			},
		})
		if err != nil {
			panic(fmt.Sprintf("%+v", err))
		}
	}

	// added some jitter to avoid request storms to ddb
	time.Sleep(time.Second * time.Duration(rand.Float32()))
	tick := time.NewTicker(time.Second)

	for {
		select {
		case <-ctx.Done():
			update()
			return
		case <-tick.C:
			update()
		}
	}
}

type CountReader struct {
	io.Reader
	count int64
}

func (c *CountReader) Read(p []byte) (n int, err error) {
	n, err = c.Reader.Read(p)
	atomic.AddInt64(&c.count, int64(n))
	return
}
