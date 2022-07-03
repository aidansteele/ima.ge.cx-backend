package main

import (
	"browseimage/bitypes"
	"browseimage/layerreader"
	"browseimage/targzi"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
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
	fileMap := map[string]*entryWithLayer{}

	for _, hash := range input.Layers {
		w := manager.NewWriteAtBuffer(nil)
		_, err := ll.downloader.Download(ctx, w, &s3.GetObjectInput{
			Bucket: &ll.bucket,
			Key:    aws.String(fmt.Sprintf("layers/%s/files.json.gz", hash)),
		})
		if err != nil {
			return nil, fmt.Errorf(": %w", err)
		}

		gzr, err := gzip.NewReader(bytes.NewReader(w.Bytes()))
		if err != nil {
			return nil, fmt.Errorf(": %w", err)
		}

		scan := bufio.NewScanner(gzr)
		for scan.Scan() {
			e := targzi.Entry{}
			err = json.Unmarshal(scan.Bytes(), &e)
			if err != nil {
				return nil, fmt.Errorf(": %w", err)
			}

			base := filepath.Base(e.Hdr.Name)
			if base == layerreader.WhiteoutOpaqueDir {
				deletes := []string{}
				deletedDir := strings.TrimSuffix(e.Hdr.Name, layerreader.WhiteoutOpaqueDir)
				fmt.Printf("need to delete dir %s\n", deletedDir)
				for key := range fileMap {
					if strings.HasPrefix(key, deletedDir) {
						deletes = append(deletes, key)
					}
				}
				for _, del := range deletes {
					delete(fileMap, del)
					fmt.Printf("deleting (recursively) %s\n", del)
				}
			} else if strings.HasPrefix(base, layerreader.WhiteoutPrefix) {
				name := strings.TrimPrefix(base, layerreader.WhiteoutPrefix)
				delete(fileMap, name)
				fmt.Printf("deleting %s\n", name)
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
		return nil, fmt.Errorf(": %w", err)
	}

	upload, err := ll.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &ll.bucket,
		Key:    aws.String(fmt.Sprintf("images/%s/%s/index.json.gz", input.Key.Repo, input.Key.Digest)),
		Body:   buf,
	})
	if err != nil {
		return nil, fmt.Errorf(": %w", err)
	}

	return &concatenatorOutput{
		Key:       *upload.Key,
		VersionId: *upload.VersionID,
	}, nil
}
