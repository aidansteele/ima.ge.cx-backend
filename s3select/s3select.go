package s3select

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
)

func Select[T any](ctx context.Context, api *s3.Client, bucket, key, query string) ([]T, error) {
	sel, err := api.SelectObjectContent(ctx, &s3.SelectObjectContentInput{
		Bucket:         &bucket,
		Key:            &key,
		Expression:     &query,
		ExpressionType: types.ExpressionTypeSql,
		InputSerialization: &types.InputSerialization{
			CompressionType: types.CompressionTypeGzip,
			JSON:            &types.JSONInput{Type: types.JSONTypeLines},
		},
		OutputSerialization: &types.OutputSerialization{
			JSON: &types.JSONOutput{RecordDelimiter: aws.String("\n")},
		},
		RequestProgress: &types.RequestProgress{
			Enabled: true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf(": %w", err)
	}

	pr, pw := io.Pipe()
	resultsch := make(chan []T, 1)

	go func() {
		entries := []T{}

		scan := bufio.NewScanner(pr)
		for scan.Scan() {
			var t T
			line := scan.Bytes()
			err = json.Unmarshal(line, &t)
			if err != nil {
				panic(fmt.Errorf(": %w", err))
			}

			entries = append(entries, t)
		}

		resultsch <- entries
	}()

	stream := sel.GetStream()
	events := stream.Events()

	for {
		select {
		case <-ctx.Done():
			stream.Close()
			return nil, ctx.Err()
		case ev, more := <-events:
			if !more {
				goto done
			}
			switch ev := ev.(type) {
			case *types.SelectObjectContentEventStreamMemberStats:
				//spew.Dump("stats", ev.Value.Details)
			case *types.SelectObjectContentEventStreamMemberProgress:
				//spew.Dump("progress", ev.Value.Details)
			case *types.SelectObjectContentEventStreamMemberCont:
				//spew.Dump("cont")
			case *types.SelectObjectContentEventStreamMemberEnd:
				//spew.Dump("end")
			case *types.SelectObjectContentEventStreamMemberRecords:
				//spew.Dump("records", ev.Value.Payload)
				pw.Write(ev.Value.Payload)
			default:
				panic("unexpected event type")
			}
		}
	}
done:

	pw.Close()
	return <-resultsch, nil
}
