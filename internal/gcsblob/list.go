package gcsblob

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type ObjectInfo struct {
	Name       string
	Generation int64
	Size       int64
}

type ObjectPage struct {
	Objects       []ObjectInfo
	NextPageToken string
}

// ListObjects returns one page of current object versions. Multiple pages are
// not an atomic snapshot; callers must establish their own publication fence.
func (c *Client) ListObjects(ctx context.Context, prefix, pageToken string) (ObjectPage, error) {
	query := url.Values{
		"prefix": {prefix}, "pageToken": {pageToken}, "maxResults": {"1000"},
		"fields": {"nextPageToken,items(name,generation,size)"},
	}
	u := c.storageBase + "/b/" + url.PathEscape(c.bucket) + "/o?" + query.Encode()
	req, err := c.newReq(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ObjectPage{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return ObjectPage{}, err
	}
	defer drainClose(resp)
	if resp.StatusCode != http.StatusOK {
		return ObjectPage{}, fmt.Errorf("list %s: HTTP %d", prefix, resp.StatusCode)
	}
	var body struct {
		NextPageToken string           `json:"nextPageToken"`
		Items         []objectMetadata `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ObjectPage{}, fmt.Errorf("decode object listing: %w", err)
	}
	page := ObjectPage{NextPageToken: body.NextPageToken}
	for _, item := range body.Items {
		if item.Name == "" || !strings.HasPrefix(item.Name, prefix) {
			return ObjectPage{}, fmt.Errorf("listed object is outside requested prefix")
		}
		info, err := item.info()
		if err != nil {
			return ObjectPage{}, err
		}
		page.Objects = append(page.Objects, info)
	}
	return page, nil
}
