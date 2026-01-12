package targzi

import (
	"browseimage/s3select"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type TarExplorer struct {
	s3          *s3.Client
	downloader  *manager.Downloader
	bucket      string
	gzIndexPath string
}

func NewTarExplorer(s3 *s3.Client, bucket string) *TarExplorer {
	return &TarExplorer{
		s3:         s3,
		downloader: manager.NewDownloader(s3),
		bucket:     bucket,
	}
}

func (te *TarExplorer) FileContents(ctx context.Context, key, file string) ([]byte, error) {
	if te.gzIndexPath == "" {
		indexFile, err := os.CreateTemp("", "index*")
		if err != nil {
			return nil, fmt.Errorf("making file for index: %w", err)
		}

		_, err = te.downloader.Download(ctx, indexFile, &s3.GetObjectInput{
			Bucket: &te.bucket,
			Key:    aws.String(fmt.Sprintf("%s/index.gzi", key)),
		})
		if err != nil {
			return nil, fmt.Errorf("downloading index file: %w", err)
		}

		err = indexFile.Close()
		if err != nil {
			return nil, fmt.Errorf("closing index file: %w", err)
		}

		te.gzIndexPath = indexFile.Name()
	}

	// Validate input to prevent SQL injection - reject quotes entirely
	if strings.ContainsRune(file, '\'') {
		return nil, fmt.Errorf("invalid character in file path")
	}
	escapedFile := strings.ReplaceAll(file, "'", "''")
	query := fmt.Sprintf("SELECT * FROM s3object s WHERE s.Hdr.Name = '%s'", escapedFile)
	entries, err := s3select.Select[Entry](ctx, te.s3, te.bucket, fmt.Sprintf("%s/files.json.gz", key), query)
	if err != nil {
		return nil, fmt.Errorf("querying file index: %w", err)
	}

	if len(entries) != 1 {
		return nil, fmt.Errorf("unexpected number of entries: %d", len(entries))
	}
	entry := entries[0]

	if len(entry.Spans) == 0 {
		return nil, fmt.Errorf("entry has no spans")
	}

	c := &http.Client{
		//Transport: &transport{RoundTripper: http.DefaultTransport},
	}
	// TODO: This URL is hardcoded for testing - needs registry info to work properly
	req, err := http.NewRequest("GET", "https://mcr.microsoft.com/v2/dotnet/sdk/blobs/sha256:a603fa5e3b4127f210503aaa6189abf6286ee5a73deeaab460f8f33ebc6b64e2", nil)
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}

	spans, err := Spans(te.gzIndexPath)
	if err != nil {
		return nil, fmt.Errorf("reading spans: %w", err)
	}

	//span := IndexSpan{}
	start := 0
	end := 0
	firstSpan := entry.Spans[0]
	lastSpan := entry.Spans[len(entry.Spans)-1]
	for idx, s := range spans {
		if s.Number == firstSpan {
			start = s.Compressed
		}
		if s.Number == lastSpan {
			if idx+1 < len(spans) {
				end = spans[idx+1].Compressed
			}
		}
	}

	rangeHdr := fmt.Sprintf("bytes=%d-", start-1)
	if end > 0 {
		rangeHdr += fmt.Sprintf("%d", end)
	}
	req.Header.Set("Range", rangeHdr)

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching layer content: %w", err)
	}

	defer resp.Body.Close()
	extracted, err := Extract(ctx, resp.Body, te.gzIndexPath, start, entry.Offset, int(entry.Hdr.Size))
	if err != nil {
		return nil, fmt.Errorf("extracting: %w", err)
	}

	return extracted, nil
}

func (te *TarExplorer) ListDirectory(ctx context.Context, key, dir string) ([]Entry, error) {
	// Validate input to prevent SQL injection - reject quotes entirely
	if strings.ContainsRune(dir, '\'') {
		return nil, fmt.Errorf("invalid character in directory path")
	}
	escapedDir := strings.ReplaceAll(dir, "'", "''")
	query := fmt.Sprintf("SELECT * FROM s3object s WHERE s.Parent = '%s'", escapedDir)
	return s3select.Select[Entry](ctx, te.s3, te.bucket, fmt.Sprintf("%s/files.json.gz", key), query)
}

type transport struct {
	http.RoundTripper
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	dump, _ := httputil.DumpRequestOut(request, false)
	slog.DebugContext(ctx, "outgoing HTTP request", "dump", string(dump))

	response, err := t.RoundTripper.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	dump, _ = httputil.DumpResponse(response, false)
	slog.DebugContext(ctx, "incoming HTTP response", "dump", string(dump))
	return response, nil
}
