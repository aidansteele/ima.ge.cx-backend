package targzi

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-xray-sdk-go/xray"
	"github.com/klauspost/compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

type Entry struct {
	Offset int
	Spans  []int
	Hdr    tar.Header
	Parent string
}

type Index struct {
	Entries       []*Entry
	GzIndexPath   string
	FileIndexPath string
}

type IndexSpan struct {
	Number       int
	Uncompressed int
	Compressed   int
}

func (i *Index) Spans() ([]IndexSpan, error) {
	return Spans(i.GzIndexPath)
}

func ReadIndex(dir string) (*Index, error) {
	gzIndexPath := fmt.Sprintf("%s/index.gzi", dir)
	_, err := Spans(gzIndexPath)
	if err != nil {
		return nil, fmt.Errorf("reading index spans: %w", err)
	}

	fileIndexPath := fmt.Sprintf("%s/files.json.gz", dir)
	findex, err := os.Open(fileIndexPath)
	if err != nil {
		return nil, fmt.Errorf("opening file index: %w", err)
	}

	gzr, err := gzip.NewReader(findex)
	if err != nil {
		return nil, fmt.Errorf("gunzipping file index: %w", err)
	}

	entries := []*Entry{}

	scan := bufio.NewScanner(gzr)
	for scan.Scan() {
		e := Entry{}
		err = json.Unmarshal(scan.Bytes(), &e)
		if err != nil {
			return nil, fmt.Errorf(": %w", err)
		}

		entries = append(entries, &e)
	}

	return &Index{
		Entries:       entries,
		GzIndexPath:   gzIndexPath,
		FileIndexPath: fileIndexPath,
	}, nil
}

func (i *Index) Extract(ctx context.Context, gz io.Reader, skipped, uncompressedOffset, length int) ([]byte, error) {
	return Extract(ctx, gz, i.GzIndexPath, skipped, uncompressedOffset, length)
}

func Extract(ctx context.Context, gz io.Reader, gzIndexPath string, skipped, uncompressedOffset, length int) ([]byte, error) {
	_, seg := xray.BeginSubsegment(ctx, "gztool")
	defer seg.Close(nil)

	cmd := exec.Command(
		"gztool",
		"-I", gzIndexPath,
		"-n", fmt.Sprintf("%d", skipped),
		"-b", fmt.Sprintf("%d", uncompressedOffset),
		"-r", fmt.Sprintf("%d", length),
	)

	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = gz

	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("running gztool: %w", err)
	}

	return stdout.Bytes(), nil
}

func BuildIndex(root string, gz io.Reader, fileCounter *int64) (*Index, error) {
	dir, err := os.MkdirTemp(root, "targzi*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	fileIndexPath := fmt.Sprintf("%s/files.json.gz", dir)
	findex, err := os.Create(fileIndexPath)
	if err != nil {
		return nil, fmt.Errorf("creating file index: %w", err)
	}
	defer findex.Close()

	gzIndexPath := fmt.Sprintf("%s/index.gzi", dir)

	cmd := exec.Command("gztool", "-I", gzIndexPath, "-b", "0")
	cmd.Stdin = gz
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	err = cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("starting gztool: %w", err)
	}

	entries := []*Entry{}

	off := &offsetReporter{Reader: stdout}
	tr := tar.NewReader(off)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return nil, fmt.Errorf("iterating tar file: %w", err)
		}

		//idx := strings.LastIndex(hdr.Name, "/")
		//hdr.Name = hdr.Name[:idx] + "#" + hdr.Name[idx+1:]

		hdr.Name = filepath.Clean(hdr.Name)
		if hdr.Name == "." {
			hdr.Name = "/"
		}

		parent := filepath.Dir(hdr.Name)
		if parent == "." {
			parent = "/"
		} else if parent != "/" {
			parent += "/"
		}

		if hdr.Name != "/" && hdr.FileInfo().IsDir() {
			hdr.Name += "/"
		}

		entries = append(entries, &Entry{
			Hdr:    *hdr,
			Offset: off.offset,
			Parent: parent,
		})

		atomic.AddInt64(fileCounter, 1)
	}

	sort.Slice(entries, func(i, j int) bool {
		iname := strings.TrimSuffix(entries[i].Hdr.Name, "/")
		isplit := strings.Split(iname, "/")

		jname := strings.TrimSuffix(entries[j].Hdr.Name, "/")
		jsplit := strings.Split(jname, "/")

		if len(isplit) == len(jsplit) {
			return iname < jname
		} else {
			return len(isplit) < len(jsplit)
		}
		//ip, is, _ := strings.Cut(entries[i].Hdr.Name, "#")
		//jp, js, _ := strings.Cut(entries[j].Hdr.Name, "#")
		//
		//if ip == jp {
		//	return is < js
		//} else {
		//	return ip < jp
		//}
		//
		////return entries[i].Hdr.Name < entries[j].Hdr.Name
	})

	err = cmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("gztool exit: %w", err)
	}

	spans, err := Spans(gzIndexPath)
	if err != nil {
		return nil, fmt.Errorf("Spans: %w", err)
	}

	sort.Slice(spans, func(i, j int) bool {
		return spans[i].Uncompressed > spans[j].Uncompressed
	})

	for _, entry := range entries {
		firstSpan := sort.Search(len(spans), func(i int) bool {
			return entry.Offset >= spans[i].Uncompressed
		})

		lastSpan := sort.Search(len(spans), func(i int) bool {
			return entry.Offset+int(entry.Hdr.Size) >= spans[i].Uncompressed
		})

		for idx := lastSpan; idx <= firstSpan; idx++ {
			entry.Spans = append(entry.Spans, spans[idx].Number)
		}

		sort.Ints(entry.Spans)
	}

	gzw := gzip.NewWriter(findex)

	for _, entry := range entries {
		j, _ := json.Marshal(entry)
		gzw.Write(j)
		gzw.Write([]byte{'\n'})
	}

	err = gzw.Close()
	if err != nil {
		return nil, fmt.Errorf("closing file index: %w", err)
	}

	return &Index{
		Entries:       entries,
		GzIndexPath:   gzIndexPath,
		FileIndexPath: fileIndexPath,
	}, nil
}

func Spans(path string) ([]IndexSpan, error) {
	stdout := &bytes.Buffer{}

	cmd := exec.Command("gztool", "-ll", path)
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("running gztool introspection: %w", err)
	}

	spans := []IndexSpan{}

	re := regexp.MustCompile(`#(\d+): @ (\d+) / (\d+)`)
	matches := re.FindAllStringSubmatch(stdout.String(), -1)
	for _, m := range matches {
		number, _ := strconv.Atoi(m[1])
		compressed, _ := strconv.Atoi(m[2])
		uncompressed, _ := strconv.Atoi(m[3])

		spans = append(spans, IndexSpan{
			Number:       number,
			Uncompressed: uncompressed,
			Compressed:   compressed,
		})
	}

	return spans, nil
}

type offsetReporter struct {
	io.Reader
	offset int
}

func (o *offsetReporter) Read(p []byte) (n int, err error) {
	n, err = o.Reader.Read(p)
	o.offset += n
	return
}
