package quark

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/go-resty/resty/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	conf.Conf = conf.DefaultConfig()
	dbConn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	db.Init(dbConn)
	os.Exit(m.Run())
}

var testDriverSeq int

func newTestDriver(srvURL string) *QuarkOrUC {
	testDriverSeq++
	d := &QuarkOrUC{Storage: model.Storage{MountPath: fmt.Sprintf("test-%d", testDriverSeq)}}
	d.conf = Conf{
		ua:      "test-ua",
		referer: "https://pan.quark.cn",
		api:     srvURL + "/1/clouddrive",
		pr:      "ucpro",
	}
	d.client = resty.New()
	return d
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestRequestIgnoresSetCookieWithoutPuus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "st=new; Path=/")
		writeJSON(w, http.StatusOK, Resp{Status: 200, Code: 0, Message: "ok"})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "__puus=old; st=old"
	if _, err := d.request("/config", http.MethodGet, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Cookie, "__puus=old") || !strings.Contains(d.Cookie, "st=old") || strings.Contains(d.Cookie, "st=new") {
		t.Fatalf("cookie changed without __puus: %q", d.Cookie)
	}
}

func TestRequestMergesAllSetCookiesWhenPuusPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "__puus=new; Path=/")
		w.Header().Add("Set-Cookie", "st=new-st; Path=/")
		w.Header().Add("Set-Cookie", "tfstk=new-tfstk; Path=/")
		writeJSON(w, http.StatusOK, Resp{Status: 200, Code: 0, Message: "ok"})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "__puus=old; st=old-st; keep=yes"
	if _, err := d.request("/config", http.MethodGet, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"__puus=new", "st=new-st", "tfstk=new-tfstk", "keep=yes"} {
		if !strings.Contains(d.Cookie, want) {
			t.Fatalf("cookie missing %q: %q", want, d.Cookie)
		}
	}
	if strings.Contains(d.Cookie, "__puus=old") || strings.Contains(d.Cookie, "st=old-st") {
		t.Fatalf("old cookie value remained: %q", d.Cookie)
	}
}

// TestGetDownloadLinkUsesResponseMergedCookie 下载头必须使用 /file/download 响应合并后的最新 cookie，
// 与 JS SDK 的 createLink(config.cookie) 时序一致：响应 Set-Cookie 中的 __puus 就是 download_url 绑定的会话状态。
func TestGetDownloadLinkUsesResponseMergedCookie(t *testing.T) {
	var pathsMu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		if r.URL.Path != "/1/clouddrive/file/download" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Cookie"); got != "__puus=before" {
			t.Errorf("download API Cookie = %q", got)
		}
		w.Header().Add("Set-Cookie", "__puus=after; Path=/")
		w.Header().Add("Set-Cookie", "st=st-after; Path=/")
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  200,
			"code":    0,
			"message": "ok",
			"data": []map[string]string{{
				"download_url": "https://cdn.example/file?signature=test",
			}},
		})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "__puus=before"
	link, err := d.getDownloadLink(&File{Fid: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	// 响应合并后的 cookie（含配套字段）用于下载头
	if got := link.Header.Get("Cookie"); !strings.Contains(got, "__puus=after") || !strings.Contains(got, "st=st-after") {
		t.Fatalf("link Cookie = %q, want response-merged cookie", got)
	}
	// 驱动内 cookie 同步更新
	if got := d.Cookie; !strings.Contains(got, "__puus=after") || !strings.Contains(got, "st=st-after") {
		t.Fatalf("driver Cookie = %q", got)
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if len(paths) != 1 || paths[0] != "/1/clouddrive/file/download" {
		t.Fatalf("unexpected request paths: %v", paths)
	}
}

func TestGetDownloadLinkDoesNotRefreshBeforeDownload(t *testing.T) {
	var pathsMu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		if r.URL.Path != "/1/clouddrive/file/download" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": 200,
			"code":   0,
			"data":   []map[string]string{{"download_url": "https://cdn.example/file"}},
		})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "__puus=stable"
	if _, err := d.getDownloadLink(&File{Fid: "f1"}); err != nil {
		t.Fatal(err)
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if len(paths) != 1 || paths[0] != "/1/clouddrive/file/download" {
		t.Fatalf("unexpected request paths: %v", paths)
	}
}

var _ model.Obj = (*File)(nil)
