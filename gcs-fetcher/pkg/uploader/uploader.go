/*
Copyright 2018 Google, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/api/googleapi"
)

type GCSUploader struct {
	GCS                        GCS
	OS                         OS
	Root, Bucket, ManifestFile string

	manifest map[string]sourceInfo

	totalBytes, bytesSkipped int64
}

type sourceInfo struct {
	SourceURL string      `json:"sourceUrl"`
	SHA256    string      `json:"sha256"`
	FileMode  os.FileMode `json:"mode"`
}

type OS interface {
	Walk(root string, fn filepath.WalkFunc) error
	EvalSymlinks(path string) (string, error)
	Stat(path string) (os.FileInfo, error)
}

type GCS interface {
	NewWriter(ctx context.Context, bucket, object string) io.WriteCloser
}

func (u *GCSUploader) Upload(ctx context.Context) (string, error) {
	u.manifest = map[string]sourceInfo{}

	if err := u.OS.Walk(u.Root, func(path string, info os.FileInfo, err error) error {
		return u.processFile(ctx, path, info, err)
	}); err != nil {
		return "", err
	}

	fmt.Printf(`
******************************************************
* Uploaded %d bytes (%.2f%% incremental)
******************************************************
`, u.totalBytes-u.bytesSkipped, float64(100*u.bytesSkipped/u.totalBytes))
	return u.writeManifest(ctx)
}

func (u *GCSUploader) processFile(ctx context.Context, path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}

	// Follow symlinks.
	if spath, err := u.OS.EvalSymlinks(path); err != nil {
		return err
	} else if spath != path {
		log.Printf("Path %q is symlink to %q", path, spath)
		info, err = u.OS.Stat(spath)
		if err != nil {
			return err
		}
		path = spath
	}

	// Don't process dirs.
	if info.IsDir() {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Compute digest of file, and count bytes.
	cw := &countWriter{}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(cw, h), f); err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", h.Sum(nil))

	// Seek back to the beginning of the file, to write it to GCS.
	// NB: The GCS client is responsible for skipping writes if the file
	// already exists.
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	wc := u.GCS.NewWriter(ctx, u.Bucket, digest)
	if _, err := io.Copy(wc, f); err != nil {
		return err
	}

	bytes := cw.b

	u.manifest[path] = sourceInfo{
		SourceURL: fmt.Sprintf("gs://%s/%s", u.Bucket, digest),
		SHA256:    digest,
		FileMode:  info.Mode(),
	}

	if err := wc.Close(); isAlreadyExists(err) {
		u.bytesSkipped += bytes
	} else if err != nil {
		return err
	}
	u.totalBytes += bytes
	return nil
}

type countWriter struct {
	b int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	c.b += int64(len(b))
	return len(b), nil
}

func isAlreadyExists(err error) bool {
	if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == http.StatusPreconditionFailed {
		return true
	}
	return false
}

func (u *GCSUploader) writeManifest(ctx context.Context) (string, error) {
	if u.ManifestFile == "" {
		u.ManifestFile = fmt.Sprintf("manifest-%s.json", time.Now().Format(time.RFC3339))
	}
	wc := u.GCS.NewWriter(ctx, u.Bucket, u.ManifestFile)
	uri := fmt.Sprintf("gs://%s/%s", u.Bucket, u.ManifestFile)
	if err := json.NewEncoder(wc).Encode(u.manifest); err != nil {
		return "", err
	}
	if err := wc.Close(); err != nil {
		return "", err
	}
	return uri, nil
}
