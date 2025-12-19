package main

import (
	"browseimage/bitypes"
	"browseimage/handlehttp"
	"browseimage/layerreader"
	"browseimage/s3select"
	"browseimage/targzi"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-xray-sdk-go/instrumentation/awsv2"
	"github.com/aws/aws-xray-sdk-go/xray"
	"github.com/aws/smithy-go"
	"github.com/glassechidna/go-emf/emf"
	"github.com/glassechidna/go-emf/emf/unit"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/gorilla/mux"
	"github.com/oklog/ulid/v2"
	"io"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"oras.land/oras-go/v2/registry/remote/auth"
	"os"
	"strings"
	"time"
)

func main() {
	ctx := context.Background()

	emf.Namespace = "browseimage"

	cfg, err := config.LoadDefaultConfig(ctx) //config.WithClientLogMode(aws.LogRequest|aws.LogResponse),

	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	awsv2.AWSV2Instrumentor(&cfg.APIOptions)

	h := &handler{
		s3:     s3.NewFromConfig(cfg),
		bucket: os.Getenv("BUCKET"),
		http: &auth.Client{
			//Client: xray.Client(&http.Client{Transport: &transport{RoundTripper: http.DefaultTransport}}),
			Client: xray.Client(http.DefaultClient),
			Cache:  auth.DefaultCache,
		},
		transport: xray.RoundTripper(http.DefaultTransport),
		dynamodb:  dynamodb.NewFromConfig(cfg),
		table:     os.Getenv("TABLE"),
		sfn:       sfn.NewFromConfig(cfg),
		machine:   os.Getenv("MACHINE"),
		entropy:   ulid.Monotonic(rand.New(rand.NewSource(time.Now().UnixNano())), 0),
	}

	r := mux.NewRouter()
	r.HandleFunc("/api/dir", h.handleListDirectory)
	r.HandleFunc("/api/file", h.handleFileContents)
	r.HandleFunc("/api/info", h.handleInfo)
	r.HandleFunc("/api/lookup", h.handleLookup)

	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			version := os.Getenv("AWS_LAMBDA_FUNCTION_VERSION")

			query := map[string]string{}
			for k, vs := range r.URL.Query() {
				query[k] = vs[0]
			}

			defer func() {
				rerr := recover()

				duration := time.Now().Sub(start)
				msi := emf.MSI{
					"FunctionVersion": version,
					"Path":            r.URL.Path,
					"Query":           query,
					"TraceId":         os.Getenv("_X_AMZN_TRACE_ID"),
					"Milliseconds":    emf.Metric(float64(duration.Milliseconds()), unit.Milliseconds),
				}
				defer emf.Emit(msi)

				if rerr != nil {
					msi["Error"] = fmt.Sprintf("%+v", rerr)
					panic(rerr)
				}
			}()

			w.Header().Set("Function-Version", version)
			w.Header().Set("Function-Region", os.Getenv("AWS_REGION"))
			h.ServeHTTP(w, r)
		})
	})

	if _, ok := os.LookupEnv("_HANDLER"); ok {
		lambda.Start(handlehttp.WrapHandler(r))
	} else {
		err = http.ListenAndServe(":8080", r)
		panic(err)
	}
}

type handler struct {
	s3        *s3.Client
	bucket    string
	http      *auth.Client
	transport http.RoundTripper
	dynamodb  *dynamodb.Client
	table     string
	sfn       *sfn.Client
	machine   string
	entropy   io.Reader
}

type lookupOutput struct {
	Error   string        `json:",omitempty"`
	Options []imageOption `json:",omitempty"`
}

type imageOption struct {
	Digest   v1.Hash
	Platform *v1.Platform
}

func (h *handler) remoteOptions(ctx context.Context) []remote.Option {
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithTransport(h.transport),
		remote.WithUserAgent("site/ima.ge.cx author/aidan@awsteele.com"),
	}
}

func (h *handler) handleLookup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	imageQuery := r.URL.Query().Get("image")

	writeOutput := func(output lookupOutput) {
		j, _ := json.Marshal(output)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Type", "application/json")
		w.Write(j)
	}

	ref, err := name.ParseReference(imageQuery)
	if err != nil {
		writeOutput(lookupOutput{Error: err.Error()})
		return
	}

	opts := []imageOption{}

	index, err := remote.Index(ref, h.remoteOptions(ctx)...)
	if err != nil {
		var es1 *remote.ErrSchema1
		if errors.As(err, &es1) {
			image, err := remote.Image(ref, h.remoteOptions(ctx)...)
			if err != nil {
				writeOutput(lookupOutput{Error: "look ma, no hands"})
				return
			}

			digest, _ := image.Digest()
			cf, _ := image.ConfigFile()

			opts = append(opts, imageOption{
				Digest: digest,
				Platform: &v1.Platform{
					Architecture: cf.Architecture,
					OS:           cf.OS,
					OSVersion:    cf.OSVersion,
					Variant:      cf.Variant,
				},
			})

			writeOutput(lookupOutput{Options: opts})
			return
		}

		writeOutput(lookupOutput{Error: err.Error()})
		return
	}

	im, err := index.IndexManifest()
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	for _, manifest := range im.Manifests {
		opts = append(opts, imageOption{
			Digest:   manifest.Digest,
			Platform: manifest.Platform,
		})
	}

	writeOutput(lookupOutput{Options: opts})
}

type LayerProgress struct {
	Layer          string
	TotalBytes     int64
	CompletedBytes int64
	TotalFiles     int64
	CompletedFiles int64
}

type HandleImageOutput struct {
	Status          bitypes.ImageInfoStatus
	Repo            string          `json:",omitempty"`
	Digest          string          `json:",omitempty"`
	ExecutionId     string          `json:",omitempty"`
	Progresses      []LayerProgress `json:",omitempty"`
	TotalSize       int64           `json:",omitempty"`
	CompletedSize   int64           `json:",omitempty"`
	EstimateSeconds int64           `json:",omitempty"`
	DurationSeconds float64         `json:",omitempty"`
	Retrieved       time.Time       `json:",omitempty"`
	Config          json.RawMessage `json:",omitempty"`
	Manifest        json.RawMessage `json:",omitempty"`
}

func estimateSeconds(totalSize int64) int64 {
	return 2 + (totalSize / 25e6)
}

func (h *handler) handleStartExecution(key *bitypes.ImageInfoKey, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	executionId := fmt.Sprintf("BI%s10", ulid.MustNew(ulid.Timestamp(time.Now()), h.entropy))

	item := &bitypes.ImageInfoItem{
		ImageInfoKey: *key,
		ExecutionId:  executionId,
		Status:       "PENDING",
		Retrieved:    time.Now(),
	}

	_, err := h.dynamodb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &h.table,
		Item:                item.DynamoItem(),
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	sfnInput, _ := json.Marshal(key)

	_, err = h.sfn.StartExecution(ctx, &sfn.StartExecutionInput{
		StateMachineArn: &h.machine,
		Name:            &executionId,
		TraceHeader:     aws.String(os.Getenv("_X_AMZN_TRACE_ID")),
		Input:           aws.String(string(sfnInput)),
	})
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	_, err = h.dynamodb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                &h.table,
		Key:                      item.Key(),
		UpdateExpression:         aws.String("SET #status = :status"),
		ConditionExpression:      aws.String("ExecutionId = :executionId"),
		ExpressionAttributeNames: map[string]string{"#status": "Status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":executionId": &types.AttributeValueMemberS{Value: executionId},
			":status":      &types.AttributeValueMemberS{Value: "RUNNING"},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	j, _ := json.Marshal(item)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(j)
}

func (h *handler) handleInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	q := r.URL.Query()
	image := q.Get("image")
	repo, _, _ := strings.Cut(image, ":") // drop tag (if any)
	digest := q.Get("digest")

	p := dynamodb.NewQueryPaginator(h.dynamodb, &dynamodb.QueryInput{
		TableName:              &h.table,
		ConsistentRead:         aws.Bool(true),
		KeyConditionExpression: aws.String("pk = :pk and begins_with(sk, :sk)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("image#%s", repo)},
			":sk": &types.AttributeValueMemberS{Value: fmt.Sprintf("digest#%s", digest)},
		},
	})

	imageInfo := bitypes.ImageInfoItem{}
	progresses := []LayerProgress{}
	var completedSize int64 = 0

	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			panic(fmt.Sprintf("%+v", err))
		}

		for _, item := range page.Items {
			sk := strings.Split(item["sk"].(*types.AttributeValueMemberS).Value, "#")
			if len(sk) == 2 {
				err = attributevalue.UnmarshalMap(item, &imageInfo)
				if err != nil {
					panic(fmt.Sprintf("%+v", err))
				}
			} else if len(sk) == 4 {
				lp := bitypes.LayerProgress{}
				err = attributevalue.UnmarshalMap(item, &lp)
				if err != nil {
					panic(fmt.Sprintf("%+v", err))
				}
				progresses = append(progresses, LayerProgress{
					Layer:          lp.LayerDigest,
					TotalBytes:     lp.TotalBytes,
					CompletedBytes: lp.CompletedBytes,
					TotalFiles:     lp.TotalFiles,
					CompletedFiles: lp.CompletedFiles,
				})

				completedSize += lp.CompletedBytes
			} else {
				panic(fmt.Sprintf("unexpected dynamo item: %+v", item))
			}
		}
	}

	// image was not in dynamodb
	if imageInfo.Digest == "" {
		h.handleStartExecution(&bitypes.ImageInfoKey{Repo: repo, Digest: digest}, w, r)
		return
	}

	maxAge := time.Second
	if imageInfo.Status == bitypes.ImageInfoStatusSucceeded {
		maxAge = time.Hour * 24
	}

	output := &HandleImageOutput{
		Status:          imageInfo.Status,
		Repo:            imageInfo.Repo,
		Digest:          imageInfo.Digest,
		ExecutionId:     imageInfo.ExecutionId,
		TotalSize:       imageInfo.TotalSize,
		CompletedSize:   completedSize,
		Progresses:      progresses,
		EstimateSeconds: estimateSeconds(imageInfo.TotalSize),
		DurationSeconds: imageInfo.Duration.Seconds(),
		Retrieved:       imageInfo.Retrieved,
		Config:          imageInfo.RawConfig,
		Manifest:        imageInfo.Manifest,
	}

	j, _ := json.Marshal(output)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", maxAge/time.Second))
	w.Write(j)
}

func (h *handler) handleListDirectory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	q := r.URL.Query()

	image := q.Get("image")
	image, tag, _ := strings.Cut(image, ":") // drop tag (if any)
	digest := q.Get("digest")
	key := fmt.Sprintf("images/%s/%s/index.json.gz", image, digest)

	path := q.Get("path")
	if path == "" {
		path = "/"
	}

	msi := emf.MSI{
		"Image":      image,
		"Tag":        tag,
		"Digest":     digest,
		"Path":       path,
		"StatusCode": emf.Dimension("200"),
	}
	defer emf.Emit(msi)

	query := fmt.Sprintf("SELECT * FROM s3object s WHERE s.Parent = '%s'", path)
	entries, err := s3select.Select[layerreader.EntryWithLayer](ctx, h.s3, h.bucket, key, query)
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchKey" {
			http.NotFound(w, r)
			msi["StatusCode"] = emf.Dimension("404")
			return
		}

		msi["StatusCode"] = emf.Dimension("500")
		panic(fmt.Sprintf("%+v", err))
	}

	j, _ := json.Marshal(entries)
	w.Header().Set("Content-Type", "application/json")
	w.Write(j)
}

func (h *handler) handleFileContents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	q := r.URL.Query()

	image := q.Get("image")
	image, _, _ = strings.Cut(image, ":") // drop tag (if any)
	digest := q.Get("digest")
	key := fmt.Sprintf("images/%s/%s/index.json.gz", image, digest)
	//prefix := h.prefix(ctx, img, digest)

	path := q.Get("path")

	query := fmt.Sprintf("SELECT * FROM s3object s WHERE s.Hdr.Name = '%s'", path)
	entries, err := s3select.Select[layerreader.EntryWithLayer](ctx, h.s3, h.bucket, key, query)
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	if len(entries) != 1 {
		panic(fmt.Errorf("unexpected number of entries: %d", len(entries)))
	}
	entry := entries[0]

	get, err := h.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &h.bucket,
		Key:    aws.String(fmt.Sprintf("layers/%s/index.gzi", entry.Layer)),
	})
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	indexFile, err := os.CreateTemp("", "gzi*")
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}
	defer os.Remove(indexFile.Name())

	defer get.Body.Close()
	_, err = io.Copy(indexFile, get.Body)
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	err = indexFile.Close()
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	spans, err := targzi.Spans(indexFile.Name())
	if err != nil {
		panic(fmt.Errorf("reading spans: %w", err))
	}

	//span := IndexSpan{}
	start := 0
	end := 0
	for idx, s := range spans {
		if s.Number == entry.Spans[0] {
			start = s.Compressed
		}
		if s.Number == entry.Spans[len(entry.Spans)-1] {
			if idx+1 < len(spans) {
				end = spans[idx+1].Compressed
			}
		}
	}

	rangeHdr := fmt.Sprintf("bytes=%d-", start-1)
	if end > 0 {
		rangeHdr += fmt.Sprintf("%d", end)
	}

	ref, err := name.ParseReference(image)
	if err != nil {
		panic(err)
	}

	repo := ref.Context()
	fmt.Printf("ref = %s\n context=%s\n registry=%s\n identifier=%s\n repostr=%s\n", ref, repo, repo.Registry, ref.Identifier(), repo.RepositoryStr())
	/*
		ref = mcr.microsoft.com/dotnet/sdk:6.0@sha256:4d21c1c3147d5239c8b4d71beec3d4eebb9ff8ac96690fe5af52f779b0decea5
		 context=mcr.microsoft.com/dotnet/sdk
		 registry=mcr.microsoft.com
		 identifier=sha256:4d21c1c3147d5239c8b4d71beec3d4eebb9ff8ac96690fe5af52f779b0decea5
		 repostr=dotnet/sdk
	*/

	scope := ref.Scope("pull")
	ctx = auth.WithScopes(ctx, scope)

	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://%s/v2/%s/blobs/%s", repo.RegistryStr(), repo.RepositoryStr(), entry.Layer), nil)
	req.Header.Set("Range", rangeHdr)

	resp, err := h.http.Do(req)
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	defer resp.Body.Close()
	extracted, err := targzi.Extract(ctx, resp.Body, indexFile.Name(), start, entry.Offset, int(entry.Hdr.Size))
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	w.Header().Set("Content-Type", http.DetectContentType(extracted))
	w.Write(extracted)
}

type transport struct {
	http.RoundTripper
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	dump, _ := httputil.DumpRequestOut(request, false)
	fmt.Println(string(dump))

	response, err := t.RoundTripper.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	dump, _ = httputil.DumpResponse(response, false)
	fmt.Println(string(dump))
	return response, nil
}
