//go:build !js

package storage

import (
	"fmt"
	"os"
	"strings"

	"github.com/ai-workspace-lab/distribution/internal/config"
)

// FromSpec constructs a native storage Backend from a declarative StorageTargetSpec.
func FromSpec(name string, spec config.StorageTargetSpec) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(spec.Provider)) {
	case "", "local", "fs":
		dir := spec.LocalDir
		if dir == "" {
			dir = "./dist-data"
		}
		return NewLocalFSBackend(dir, spec.PublicBaseURL)
	case "memory", "mem":
		return NewMemoryBackend(spec.PublicBaseURL), nil
	case "r2", "cloudflare-r2", "oss", "aliyun-oss", "s3":
		ak := ""
		sk := ""
		if spec.AccessKeyEnv != "" {
			ak = os.Getenv(spec.AccessKeyEnv)
		}
		if spec.SecretKeyEnv != "" {
			sk = os.Getenv(spec.SecretKeyEnv)
		}
		endpoint := spec.Endpoint
		if strings.HasPrefix(endpoint, "$") {
			endpoint = os.Getenv(strings.TrimPrefix(endpoint, "$"))
		}
		bucket := spec.Bucket
		if strings.HasPrefix(bucket, "$") {
			bucket = os.Getenv(strings.TrimPrefix(bucket, "$"))
		}
		return NewS3Backend(S3Config{
			Name:           name,
			Endpoint:       endpoint,
			Region:         spec.Region,
			Bucket:         bucket,
			Prefix:         spec.Prefix,
			AccessKeyID:    ak,
			SecretKey:      sk,
			PublicBaseURL:  spec.PublicBaseURL,
			ForcePathStyle: spec.ForcePathStyle || strings.Contains(spec.Provider, "r2"),
		})
	default:
		return nil, fmt.Errorf("unsupported storage provider %q for target %q", spec.Provider, name)
	}
}
