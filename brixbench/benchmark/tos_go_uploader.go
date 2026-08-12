/*
Copyright 2026 The Aibrix Team.

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

package benchmark

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
)

const (
	defaultTOSEndpoint = "https://tos-cn-beijing.volces.com"
	defaultTOSRegion   = "cn-beijing"
)

// tosUploader uploads official benchmark artifacts to TOS.
// AppendBytes is used for appendable aggregate CSV objects only.
// Exists is a lightweight metadata check (Head) without downloading content.
type tosUploader interface {
	Upload(localPath, remoteURI string) error
	Download(remoteURI, localPath string) error
	Delete(remoteURI string) error
	AppendBytes(remoteURI string, data []byte) error
	Exists(remoteURI string) (bool, error)
}

type goTOSUploader struct {
	client *tos.ClientV2
}

func newGoTOSUploader() (tosUploader, error) {
	ak := strings.TrimSpace(os.Getenv("TOS_ACCESS_KEY"))
	sk := strings.TrimSpace(os.Getenv("TOS_SECRET_KEY"))
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("TOS_ACCESS_KEY and TOS_SECRET_KEY must be set to publish results (do not embed credentials in source)")
	}
	endpoint := strings.TrimSpace(os.Getenv("TOS_ENDPOINT"))
	if endpoint == "" {
		endpoint = defaultTOSEndpoint
	}
	region := strings.TrimSpace(os.Getenv("TOS_REGION"))
	if region == "" {
		region = defaultTOSRegion
	}
	client, err := tos.NewClientV2(
		endpoint,
		tos.WithRegion(region),
		tos.WithCredentials(tos.NewStaticCredentials(ak, sk)),
		tos.WithEnableCRC(true),
		tos.WithMaxRetryCount(3),
	)
	if err != nil {
		return nil, fmt.Errorf("create TOS client: %w", err)
	}
	return &goTOSUploader{client: client}, nil
}

func (u *goTOSUploader) Upload(localPath, remoteURI string) error {
	bucket, key, err := parseTOSURI(remoteURI)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, err = u.client.PutObjectV2(ctx, &tos.PutObjectV2Input{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket:        bucket,
			Key:           key,
			ContentLength: info.Size(),
		},
		Content: f,
	})
	if err != nil {
		return fmt.Errorf("tos put %s: %w", remoteURI, err)
	}
	return nil
}

func (u *goTOSUploader) Download(remoteURI, localPath string) error {
	bucket, key, err := parseTOSURI(remoteURI)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := u.client.GetObjectV2(ctx, &tos.GetObjectV2Input{Bucket: bucket, Key: key})
	if err != nil {
		return fmt.Errorf("tos get %s: %w", remoteURI, err)
	}
	defer out.Content.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, out.Content); err != nil {
		return err
	}
	return nil
}

func (u *goTOSUploader) Delete(remoteURI string) error {
	bucket, key, err := parseTOSURI(remoteURI)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err = u.client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{Bucket: bucket, Key: key})
	if err != nil {
		if isTOSNotFound(err) {
			return nil
		}
		return fmt.Errorf("tos delete %s: %w", remoteURI, err)
	}
	return nil
}

func (u *goTOSUploader) Exists(remoteURI string) (bool, error) {
	bucket, key, err := parseTOSURI(remoteURI)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err = u.client.HeadObjectV2(ctx, &tos.HeadObjectV2Input{Bucket: bucket, Key: key})
	if err == nil {
		return true, nil
	}
	if isTOSNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("tos head %s: %w", remoteURI, err)
}

func (u *goTOSUploader) AppendBytes(remoteURI string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	bucket, key, err := parseTOSURI(remoteURI)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	input := &tos.AppendObjectV2Input{
		Bucket:        bucket,
		Key:           key,
		Content:       bytes.NewReader(data),
		ContentLength: int64(len(data)),
	}
	head, headErr := u.client.HeadObjectV2(ctx, &tos.HeadObjectV2Input{Bucket: bucket, Key: key})
	if headErr == nil {
		if head.ObjectType != "" && !strings.EqualFold(head.ObjectType, "Appendable") {
			return fmt.Errorf("tos append %s: object exists but is not Appendable (object_type=%q); delete it and recreate via AppendObject", remoteURI, head.ObjectType)
		}
		input.Offset = head.ContentLength
		input.PreHashCrc64ecma = head.HashCrc64ecma
	} else if !isTOSNotFound(headErr) {
		return fmt.Errorf("tos head %s: %w", remoteURI, headErr)
	}

	_, err = u.client.AppendObjectV2(ctx, input)
	if err != nil {
		return fmt.Errorf("tos append %s: %w", remoteURI, err)
	}
	return nil
}

func parseTOSURI(remoteURI string) (bucket, key string, err error) {
	const prefix = "tos://"
	if !strings.HasPrefix(remoteURI, prefix) {
		return "", "", fmt.Errorf("invalid TOS URI %q (want tos://bucket/key)", remoteURI)
	}
	rest := strings.TrimPrefix(remoteURI, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid TOS URI %q (want tos://bucket/key)", remoteURI)
	}
	return parts[0], strings.TrimPrefix(parts[1], "/"), nil
}

func isTOSNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "nosuchkey") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such key")
}
