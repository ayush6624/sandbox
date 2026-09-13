package gcsblob

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// DeleteIfGenerationMatch removes only the observed live object. Absence is
// success; a replacement returns ErrPreconditionFailed so a collector rereads.
// The boolean is true only when this request confirmed a deletion, allowing
// concurrent collectors to count reclaimed bytes without counting absence twice.
func (c *Client) DeleteIfGenerationMatch(ctx context.Context, object string, generation int64) (bool, error) {
	if generation <= 0 {
		return false, fmt.Errorf("conditional deletion requires a positive generation")
	}
	u := c.objectURL(object) + "?ifGenerationMatch=" + strconv.FormatInt(generation, 10)
	req, err := c.newReq(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer drainClose(resp)
	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusPreconditionFailed:
		return false, ErrPreconditionFailed
	default:
		return false, fmt.Errorf("conditional delete %s: HTTP %d", object, resp.StatusCode)
	}
}
