package gcsblob

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListObjectsCarriesPrefixAndEmptyPageToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("prefix") != "chunksets/data/a/" || r.URL.Query().Get("maxResults") != "1000" {
			t.Errorf("unexpected listing request: %s", r.URL)
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			fmt.Fprint(w, `{"nextPageToken":"opaque/+ next"}`)
		case "opaque/+ next":
			fmt.Fprint(w, `{"items":[{"name":"chunksets/data/a/empty","generation":"42","size":"0"}]}`)
		default:
			t.Error("page token changed")
		}
	}))
	defer srv.Close()
	c := testClient(t, srv)
	first, err := c.ListObjects(context.Background(), "chunksets/data/a/", "")
	if err != nil || len(first.Objects) != 0 || first.NextPageToken != "opaque/+ next" {
		t.Fatalf("empty page: %+v %v", first, err)
	}
	second, err := c.ListObjects(context.Background(), "chunksets/data/a/", first.NextPageToken)
	if err != nil || second.NextPageToken != "" || len(second.Objects) != 1 || second.Objects[0].Generation != 42 || second.Objects[0].Size != 0 {
		t.Fatalf("last page: %+v %v", second, err)
	}
}

func TestListObjectsRejectsInvalidMetadata(t *testing.T) {
	for _, body := range []string{
		`{"items":[{"name":"other/object","generation":"1","size":"0"}]}`,
		`{"items":[{"name":"set/object","generation":"0","size":"0"}]}`,
		`{"items":[{"name":"set/object","generation":"1","size":"-1"}]}`,
		`{"items":[{"name":"set/object","generation":"1","size":"bad"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			if _, err := testClient(t, srv).ListObjects(context.Background(), "set/", ""); err == nil {
				t.Fatal("accepted invalid listing")
			}
		})
	}
}
