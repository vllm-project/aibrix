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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	// Default Volcengine TOS S3-compatible endpoint (not the TOS-native host).
	defaultTOSEndpoint = "https://tos-s3-cn-beijing.volces.com"
	defaultTOSRegion   = "cn-beijing"
)

// tosUploader uploads official benchmark artifacts to TOS (S3-compatible API).
// AppendBytes is used for aggregate CSV objects only (get + concat + put; S3 has
// no native AppendObject).
// Exists is a lightweight metadata check (Head) without downloading content.
type tosUploader interface {
	Upload(localPath, remoteURI string) error
	Download(remoteURI, localPath string) error
	Delete(remoteURI string) error
	AppendBytes(remoteURI string, data []byte) error
	Exists(remoteURI string) (bool, error)
}

type goTOSUploader struct {
	client *s3.Client
}

func newGoTOSUploader() (tosUploader, error) {
	ak := strings.TrimSpace(os.Getenv("TOS_ACCESS_KEY"))
	sk := strings.TrimSpace(os.Getenv("TOS_SECRET_KEY"))
	if (ak == "") != (sk == "") {
		return nil, fmt.Errorf("TOS_ACCESS_KEY and TOS_SECRET_KEY must both be set or both unset (unset uses the AWS default credential chain)")
	}
	endpoint := normalizeTOSS3Endpoint(strings.TrimSpace(os.Getenv("TOS_ENDPOINT")))
	if endpoint == "" {
		endpoint = defaultTOSEndpoint
	}
	region := strings.TrimSpace(os.Getenv("TOS_REGION"))
	if region == "" {
		region = defaultTOSRegion
	}
	forcePathStyle := envTruthy(os.Getenv("TOS_S3_FORCE_PATH_STYLE"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	loadOpts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
	}
	if ak != "" && sk != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(ak, sk, ""),
		))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load S3/TOS config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = forcePathStyle
	})
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
	_, err = u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(info.Size()),
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
	out, err := u.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("tos get %s: %w", remoteURI, err)
	}
	defer out.Body.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, out.Body); err != nil {
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
	_, err = u.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
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
	_, err = u.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isTOSNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("tos head %s: %w", remoteURI, err)
}

// AppendBytes downloads any existing object, concatenates data, and PutObject.
// S3-compatible APIs (including Volcengine TOS S3) have no AppendObject.
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

	var body []byte
	out, getErr := u.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if getErr == nil {
		defer out.Body.Close()
		existing, readErr := io.ReadAll(out.Body)
		if readErr != nil {
			return fmt.Errorf("tos get body %s: %w", remoteURI, readErr)
		}
		body = append(existing, data...)
	} else if isTOSNotFound(getErr) {
		body = data
	} else {
		return fmt.Errorf("tos get %s: %w", remoteURI, getErr)
	}

	_, err = u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return fmt.Errorf("tos append(put) %s: %w", remoteURI, err)
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

// normalizeTOSS3Endpoint rewrites Volcengine TOS-native hosts to the S3 API host.
// Non-Volc / already-S3 / internal endpoints are left unchanged (https ensured).
func normalizeTOSS3Endpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	// tos-cn-<region>.volces.com → tos-s3-cn-<region>.volces.com
	// (leave tos-s3-* alone)
	const marker = "://tos-cn-"
	const s3Marker = "://tos-s3-cn-"
	if strings.Contains(endpoint, marker) && !strings.Contains(endpoint, s3Marker) {
		endpoint = strings.Replace(endpoint, marker, s3Marker, 1)
	}
	return endpoint
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func isTOSNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nsb *types.NotFound
	if errors.As(err, &nsb) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404", "NoSuchBucket":
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status code: 404") ||
		strings.Contains(msg, "nosuchkey") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such key")
}
