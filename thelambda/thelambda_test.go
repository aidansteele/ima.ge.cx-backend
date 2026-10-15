package main

import (
	"browseimage/bitypes"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseImageQuery(t *testing.T) {
	const digest = "sha256:030eaf505bc7d39bfe1c485762603a95b5d43277f98cd3334d85e84547073abb"

	// repo is used verbatim in DynamoDB and S3 keys, so only the tag may be removed.
	for image, wantRepo := range map[string]string{
		"ubuntu":                           "ubuntu",
		"golang:1.19":                      "golang",
		"mcr.microsoft.com/dotnet/sdk:6.0": "mcr.microsoft.com/dotnet/sdk",
		"localhost:5000/team/app":          "localhost:5000/team/app",
		"localhost:5000/team/app:v1":       "localhost:5000/team/app",
	} {
		repo, ref, err := parseImageQuery(url.Values{"image": {image}, "digest": {digest}})
		require.NoError(t, err, image)
		assert.Equal(t, wantRepo, repo, image)
		assert.Equal(t, digest, ref.DigestStr(), image)
	}

	for _, q := range []url.Values{
		{"image": {"ubuntu"}},
		{"image": {"ubuntu"}, "digest": {"sha256:030e"}}, // a prefix would match other digests in DynamoDB
		{"digest": {digest}},
	} {
		_, _, err := parseImageQuery(q)
		var se *statusError
		require.True(t, errors.As(err, &se), q.Encode())
		assert.Equal(t, http.StatusBadRequest, se.status, q.Encode())
	}
}

func TestImageMetadata(t *testing.T) {
	const digest = "sha256:030eaf505bc7d39bfe1c485762603a95b5d43277f98cd3334d85e84547073abb"
	config := []byte(`{"os":"linux","architecture":"arm64","variant":"v8"}`)
	manifest := []byte(`{"layers":[{"size":1234},{"size":5678}]}`)

	for _, status := range []string{"", "SUCCEEDED", "PENDING", "RUNNING", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			// A local AWS HTTP stub makes any attempt to write an index fail.
			db := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Amz-Target") != "DynamoDB_20120810.Query" {
					t.Error("metadata request attempted a database write")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				items := []any{}
				if status != "" {
					items = append(items, map[string]any{
						"pk":          map[string]string{"S": "image#example/app"},
						"sk":          map[string]string{"S": "digest#" + digest},
						"Tags":        map[string]any{"L": []any{}},
						"RawConfig":   map[string]any{"B": config},
						"Manifest":    map[string]any{"B": manifest},
						"ExecutionId": map[string]string{"S": "test"},
						"Status":      map[string]string{"S": status},
						"TotalSize":   map[string]string{"N": "6912"},
						"Duration":    map[string]string{"N": "1000000000"},
						"Retrieved":   map[string]string{"S": "2026-10-14T00:00:00Z"},
					})
				}
				w.Header().Set("Content-Type", "application/x-amz-json-1.0")
				json.NewEncoder(w).Encode(map[string]any{"Items": items})
			}))
			defer db.Close()
			h := &handler{table: "test", dynamodb: dynamodb.New(dynamodb.Options{
				Region: "us-east-1", Credentials: aws.AnonymousCredentials{},
				EndpointResolver: dynamodb.EndpointResolverFromURL(db.URL),
			})}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/metadata?"+url.Values{
				"image": {"example/app:v1"}, "digest": {digest},
			}.Encode(), nil)
			handleErrors(h.handleMetadata)(w, r)

			if status == "" {
				assert.Equal(t, http.StatusNotFound, w.Code)
				assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				assert.Contains(t, w.Body.String(), "image has not been indexed")
				return
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			wantCache := "no-store"
			switch status {
			case "SUCCEEDED":
				wantCache = "public, max-age=86400, s-maxage=2592000"
			case "PENDING", "RUNNING":
				wantCache = "public, max-age=1"
			}
			assert.Equal(t, wantCache, w.Header().Get("Cache-Control"))
			var output HandleImageOutput
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &output))
			assert.Equal(t, bitypes.ImageInfoStatus(status), output.Status)
			assert.Equal(t, "example/app", output.Repo)
			assert.Equal(t, digest, output.Digest)
			assert.Equal(t, int64(6912), output.TotalSize)
			assert.JSONEq(t, string(config), string(output.Config))
			assert.JSONEq(t, string(manifest), string(output.Manifest))
			// The interactive endpoint shares the completed/progress policy.
			w = httptest.NewRecorder()
			handleErrors(h.handleInfo)(w, r)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, wantCache, w.Header().Get("Cache-Control"))
		})
	}
}

func TestHiddenIndexAPI(t *testing.T) {
	const digest = "sha256:030eaf505bc7d39bfe1c485762603a95b5d43277f98cd3334d85e84547073abb"
	for _, endpoint := range []string{"dir", "file"} {
		for _, hidden := range []string{"", "false", "true"} {
			t.Run(endpoint+"/"+hidden, func(t *testing.T) {
				filename := "index.json.gz"
				status := http.StatusNotFound
				message := "image has not been indexed"
				if hidden == "true" {
					filename = "hidden-index.json.gz"
					status = http.StatusConflict
					message = "Hidden files are unavailable for this older index; the image must be reindexed."
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/bucket/images/example/app/"+digest+"/"+filename {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
				}))
				defer server.Close()
				h := &handler{bucket: "bucket", s3: s3.New(s3.Options{
					Region: "us-east-1", Credentials: aws.AnonymousCredentials{},
					EndpointResolver: s3.EndpointResolverFromURL(server.URL), UsePathStyle: true,
				})}
				q := url.Values{"image": {"example/app"}, "digest": {digest}, "hidden": {hidden}, "path": {"gone"}}
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/api/"+endpoint+"?"+q.Encode(), nil)
				fn := h.handleListDirectory
				if endpoint == "file" {
					fn = h.handleFileContents
				}
				handleErrors(fn)(w, r)
				assert.Equal(t, status, w.Code)
				assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				assert.Contains(t, w.Body.String(), message)
			})
		}
	}
}

func TestRegistryCacheControl(t *testing.T) {
	for _, endpoint := range []string{"lookup", "tags"} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%t", endpoint, failure), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v2/" {
						w.WriteHeader(http.StatusOK)
						return
					}
					if failure {
						http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
						return
					}
					if endpoint == "tags" {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"name":"example/app","tags":["v1","v2"]}`)
					} else {
						w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
						io.WriteString(w, `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","size":42,"digest":"sha256:030eaf505bc7d39bfe1c485762603a95b5d43277f98cd3334d85e84547073abb","platform":{"os":"linux","architecture":"arm64"}}]}`)
					}
				}))
				defer server.Close()
				h := &handler{transport: http.DefaultTransport}
				fn := h.handleLookup
				wantCache := "public, max-age=60"
				if endpoint == "tags" {
					fn = h.handleTags
					wantCache = "public, max-age=300"
				}
				if failure {
					wantCache = "no-store"
				}
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/api/"+endpoint+"?"+url.Values{
					"image": {server.Listener.Addr().String() + "/example/app:v1"},
				}.Encode(), nil)
				handleErrors(fn)(w, r)
				require.Equal(t, http.StatusOK, w.Code)
				assert.Equal(t, wantCache, w.Header().Get("Cache-Control"))
				var output struct {
					Error   string
					Tags    []string
					Options []imageOption
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &output))
				assert.Equal(t, failure, output.Error != "", w.Body.String())
				if !failure && endpoint == "tags" {
					assert.Equal(t, []string{"v1", "v2"}, output.Tags)
				} else if !failure {
					require.Len(t, output.Options, 1)
					assert.Equal(t, "sha256:030eaf505bc7d39bfe1c485762603a95b5d43277f98cd3334d85e84547073abb", output.Options[0].Digest.String())
				}
			})
		}
	}
}

func TestFilesystemCacheControl(t *testing.T) {
	const digest = "sha256:030eaf505bc7d39bfe1c485762603a95b5d43277f98cd3334d85e84547073abb"
	for _, endpoint := range []string{"dir", "file"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%t", endpoint, missing), func(t *testing.T) {
				var stream bytes.Buffer
				encoder := eventstream.NewEncoder()
				payload := []byte(`{"Hdr":{"Name":"empty","Size":0}}` + "\n")
				if missing {
					payload = nil
				}
				for _, event := range []string{"Records", "End"} {
					var headers eventstream.Headers
					headers.Set(":message-type", eventstream.StringValue("event"))
					headers.Set(":event-type", eventstream.StringValue(event))
					require.NoError(t, encoder.Encode(&stream, eventstream.Message{Headers: headers, Payload: payload}))
					payload = nil
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
					w.Write(stream.Bytes())
				}))
				defer server.Close()
				h := &handler{bucket: "bucket", s3: s3.New(s3.Options{
					Region: "us-east-1", Credentials: aws.AnonymousCredentials{},
					EndpointResolver: s3.EndpointResolverFromURL(server.URL), UsePathStyle: true,
				})}
				fn := h.handleListDirectory
				if endpoint == "file" {
					fn = h.handleFileContents
				}
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/api/"+endpoint+"?"+url.Values{
					"image": {"example/app"}, "digest": {digest}, "path": {"empty"}, "hidden": {"true"},
				}.Encode(), nil)
				handleErrors(fn)(w, r)
				if missing && endpoint == "file" {
					assert.Equal(t, http.StatusNotFound, w.Code)
					assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				} else {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					assert.Equal(t, "public, max-age=86400, s-maxage=2592000", w.Header().Get("Cache-Control"))
					if endpoint == "file" {
						assert.Empty(t, w.Body.String())
					} else if missing {
						assert.JSONEq(t, "[]", w.Body.String())
					} else {
						assert.Contains(t, w.Body.String(), `"Name":"empty"`)
					}
				}
			})
		}
	}
}
