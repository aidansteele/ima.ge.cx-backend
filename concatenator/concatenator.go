package main

import (
	"browseimage/bitypes"
	"browseimage/layerreader"
	"browseimage/logging"
	"browseimage/targzi"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/gzip"
)

func main() {
	logging.Init()

	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	api := s3.NewFromConfig(cfg)

	c := &concatenator{
		uploader:   manager.NewUploader(api),
		downloader: manager.NewDownloader(api),
		bucket:     os.Getenv("BUCKET"),
	}
	lambda.Start(c.handle)
}

type concatenatorInput struct {
	Key    *bitypes.ImageInfoKey
	Layers []v1.Hash
}

type concatenatorOutput struct {
	Key       string
	VersionId string
}

type concatenator struct {
	uploader   *manager.Uploader
	downloader *manager.Downloader
	bucket     string
}

type entryWithLayer struct {
	targzi.Entry
	Layer string
}

func (ll *concatenator) handle(ctx context.Context, input *concatenatorInput) (*concatenatorOutput, error) {
	ctx = logging.WithRequestPayload(ctx, input)
	slog.InfoContext(ctx, "handling concatenator request")

	fileMap := map[string]*entryWithLayer{}

	for _, hash := range input.Layers {
		w := manager.NewWriteAtBuffer(nil)
		_, err := ll.downloader.Download(ctx, w, &s3.GetObjectInput{
			Bucket: &ll.bucket,
			Key:    aws.String(fmt.Sprintf("layers/%s/files.json.gz", hash)),
		})
		if err != nil {
			return nil, fmt.Errorf("downloading layer files index: %w", err)
		}

		gzr, err := gzip.NewReader(bytes.NewReader(w.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("creating gzip reader: %w", err)
		}

		scan := bufio.NewScanner(gzr)
		for scan.Scan() {
			e := targzi.Entry{}
			err = json.Unmarshal(scan.Bytes(), &e)
			if err != nil {
				return nil, fmt.Errorf("unmarshalling entry: %w", err)
			}

			base := filepath.Base(e.Hdr.Name)
			if base == layerreader.WhiteoutOpaqueDir {
				deletes := []string{}
				deletedDir := strings.TrimSuffix(e.Hdr.Name, layerreader.WhiteoutOpaqueDir)
				slog.DebugContext(ctx, "processing opaque whiteout directory", "dir", deletedDir)
				for key, entry := range fileMap {
					// Only delete entries from previous layers, not from the current layer
					if strings.HasPrefix(key, deletedDir) && entry.Layer != hash.String() {
						deletes = append(deletes, key)
					}
				}
				for _, del := range deletes {
					delete(fileMap, del)
					slog.DebugContext(ctx, "deleting recursively", "path", del)
				}
			} else if strings.HasPrefix(base, layerreader.WhiteoutPrefix) {
				name := strings.TrimPrefix(base, layerreader.WhiteoutPrefix)
				dir := filepath.Dir(e.Hdr.Name)
				fullPath := filepath.Join(dir, name)
				// Only delete if it's from a previous layer
				if existing, ok := fileMap[fullPath]; ok && existing.Layer != hash.String() {
					delete(fileMap, fullPath)
					slog.DebugContext(ctx, "deleting whiteout file", "path", fullPath)
				}
			} else {
				fileMap[e.Hdr.Name] = &entryWithLayer{Entry: e, Layer: hash.String()}
			}
		}
	}

	arr := make([]*entryWithLayer, 0, len(fileMap))
	for _, e := range fileMap {
		e := e
		arr = append(arr, e)
	}

	sort.Slice(arr, func(i, j int) bool {
		return arr[i].Hdr.Name < arr[j].Hdr.Name
	})

	buf := &bytes.Buffer{}
	gzw := gzip.NewWriter(buf)

	for _, e := range arr {
		j, _ := json.Marshal(e)
		gzw.Write(j)
		gzw.Write([]byte{'\n'})
	}

	err := gzw.Close()
	if err != nil {
		return nil, fmt.Errorf("closing gzip writer: %w", err)
	}

	upload, err := ll.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &ll.bucket,
		Key:    aws.String(fmt.Sprintf("images/%s/%s/index.json.gz", input.Key.Repo, input.Key.Digest)),
		Body:   buf,
	})
	if err != nil {
		return nil, fmt.Errorf("uploading combined index: %w", err)
	}

	return &concatenatorOutput{
		Key:       *upload.Key,
		VersionId: *upload.VersionID,
	}, nil
}
