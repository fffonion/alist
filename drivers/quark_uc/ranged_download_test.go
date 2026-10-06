package quark

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/pkg/utils"
)

func TestGetDownloadLinkUsesConfiguredRanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": 200,
			"code":   0,
			"data":   []map[string]string{{"download_url": "https://cdn.example/file"}},
		})
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name        string
		concurrency int
		partSize    int
		wantPart    int
	}{
		{name: "enabled", concurrency: 3, partSize: 10, wantPart: 10 * utils.MB},
		{name: "default-part-size", concurrency: 3, wantPart: 10 * utils.MB},
		{name: "disabled", partSize: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(srv.URL)
			d.Cookie = "__puus=test"
			d.DownConcurrency = tc.concurrency
			d.DownPartSize = tc.partSize
			link, err := d.getDownloadLink(&File{Fid: "f1"})
			if err != nil {
				t.Fatal(err)
			}
			if link.Concurrency != tc.concurrency || link.PartSize != tc.wantPart {
				t.Fatalf("range settings = (%d, %d), want (%d, %d)", link.Concurrency, link.PartSize, tc.concurrency, tc.wantPart)
			}
		})
	}
}
