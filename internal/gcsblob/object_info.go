package gcsblob

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

type objectMetadata struct {
	Name       string `json:"name"`
	Generation string `json:"generation"`
	Size       string `json:"size"`
}

func (m objectMetadata) info() (ObjectInfo, error) {
	if m.Name == "" {
		return ObjectInfo{}, fmt.Errorf("GCS object metadata has no name")
	}
	generation, err := parseGeneration(m.Generation)
	if err != nil {
		return ObjectInfo{}, err
	}
	size, err := strconv.ParseInt(m.Size, 10, 64)
	if err != nil || size < 0 {
		return ObjectInfo{}, fmt.Errorf("invalid GCS object size %q", m.Size)
	}
	return ObjectInfo{Name: m.Name, Generation: generation, Size: size}, nil
}

// Stat reads current metadata without downloading the object body.
func (c *Client) Stat(ctx context.Context, object string) (ObjectInfo, error) {
	req, err := c.newReq(ctx, http.MethodGet, c.objectURL(object)+"?fields=name,generation,size", nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer drainClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return ObjectInfo{}, ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return ObjectInfo{}, fmt.Errorf("stat %s: HTTP %d", object, resp.StatusCode)
	}
	var body objectMetadata
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ObjectInfo{}, err
	}
	if body.Name != object {
		return ObjectInfo{}, fmt.Errorf("GCS metadata name does not match requested object")
	}
	return body.info()
}

// DownloadIfGenerationMatch streams the observed current version into dst.
// A replacement fails instead of being mistaken for the object being verified.
func (c *Client) DownloadIfGenerationMatch(ctx context.Context, object string, generation int64, dst io.Writer) (int64, error) {
	if generation <= 0 {
		return 0, fmt.Errorf("conditional download requires a positive generation")
	}
	u := c.objectURL(object) + "?alt=media&ifGenerationMatch=" + strconv.FormatInt(generation, 10)
	req, err := c.newReq(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer drainClose(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return io.Copy(dst, resp.Body)
	case http.StatusNotFound:
		return 0, ErrNotExist
	case http.StatusPreconditionFailed:
		return 0, ErrPreconditionFailed
	default:
		return 0, fmt.Errorf("conditional download %s: HTTP %d", object, resp.StatusCode)
	}
}
