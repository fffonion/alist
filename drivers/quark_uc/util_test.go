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
	// 初始化内存数据库，保证 op.MustSaveDriverStorage 在测试中安全执行
	conf.Conf = conf.DefaultConfig()
	d, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	db.Init(d)
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

// recordHandler 记录收到的请求路径并转发给 handler
type recordHandler struct {
	mu      sync.Mutex
	paths   []string
	handler http.HandlerFunc
}

func (r *recordHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	r.handler(w, req)
}

func (r *recordHandler) Paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestRefreshPuus(t *testing.T) {
	rec := &recordHandler{handler: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/config" {
			http.NotFound(w, r)
			return
		}
		if c := r.Header.Get("Cookie"); strings.Contains(c, "__puus") {
			t.Errorf("refresh request must not carry __puus, got cookie: %q", c)
		}
		w.Header().Add("Set-Cookie", "__puus=newpuus; Path=/")
		writeJSON(w, 200, Resp{Status: 200, Code: 0, Message: "ok"})
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "a=1; __puus=oldpuus; b=2"

	if err := d.refreshPuus(); err != nil {
		t.Fatalf("refreshPuus: %v", err)
	}
	if !strings.Contains(d.Cookie, "__puus=newpuus") {
		t.Fatalf("cookie not refreshed: %q", d.Cookie)
	}
	if !strings.Contains(d.Cookie, "a=1") || !strings.Contains(d.Cookie, "b=2") {
		t.Fatalf("refresh dropped unrelated cookies: %q", d.Cookie)
	}
	if len(rec.Paths()) != 1 || rec.Paths()[0] != "/1/clouddrive/config" {
		t.Fatalf("unexpected requests: %v", rec.Paths())
	}
}

func TestRefreshPuusRestoresCookieOnError(t *testing.T) {
	rec := &recordHandler{handler: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 500, Resp{Status: 500, Code: 0, Message: "boom"})
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "a=1; __puus=oldpuus; b=2"

	if err := d.refreshPuus(); err == nil {
		t.Fatal("want error from refreshPuus")
	}
	if d.Cookie != "a=1; __puus=oldpuus; b=2" {
		t.Fatalf("cookie not restored after error: %q", d.Cookie)
	}
}

func TestRefreshPuusRestoresCookieWhenServerDoesNotReissue(t *testing.T) {
	rec := &recordHandler{handler: func(w http.ResponseWriter, r *http.Request) {
		// 服务端没有下发新的 __puus
		writeJSON(w, 200, Resp{Status: 200, Code: 0, Message: "ok"})
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "a=1; __puus=oldpuus; b=2"

	if err := d.refreshPuus(); err != nil {
		t.Fatalf("refreshPuus: %v", err)
	}
	if d.Cookie != "a=1; __puus=oldpuus; b=2" {
		t.Fatalf("cookie should be restored when server does not reissue: %q", d.Cookie)
	}
}

func TestRefreshPuusWithEmptyCookie(t *testing.T) {
	rec := &recordHandler{handler: func(w http.ResponseWriter, r *http.Request) {
		if c := r.Header.Get("Cookie"); c != "" {
			t.Errorf("request cookie should be empty, got %q", c)
		}
		w.Header().Add("Set-Cookie", "__puus=newpuus; Path=/")
		writeJSON(w, 200, Resp{Status: 200, Code: 0, Message: "ok"})
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = ""

	if err := d.refreshPuus(); err != nil {
		t.Fatalf("refreshPuus: %v", err)
	}
	if !strings.Contains(d.Cookie, "__puus=newpuus") {
		t.Fatalf("cookie not refreshed: %q", d.Cookie)
	}
}

func TestGetDownloadLinkUsesRequestTimeCookie(t *testing.T) {
	rec := &recordHandler{handler: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1/clouddrive/file/download":
			// 响应轮换 __puus，但下载 URL 签名基于请求时携带的 cookie
			w.Header().Add("Set-Cookie", "__puus=rotated; Path=/")
			writeJSON(w, 200, map[string]interface{}{
				"status": 200, "code": 0, "message": "ok",
				"data": []map[string]string{{"download_url": "https://cdn.example.com/f?sign=1"}},
			})
		default:
			http.NotFound(w, r)
		}
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "__puus=pre"

	link, err := d.getDownloadLink(&File{Fid: "f1"})
	if err != nil {
		t.Fatalf("getDownloadLink: %v", err)
	}
	// 下载请求头必须使用生成签名时的 cookie（快照），而不是响应更新后的值
	if got := link.Header.Get("Cookie"); got != "__puus=pre" {
		t.Fatalf("download header cookie = %q, want snapshot %q", got, "__puus=pre")
	}
	if !strings.Contains(d.Cookie, "__puus=rotated") {
		t.Fatalf("driver cookie should be updated with rotated value: %q", d.Cookie)
	}
	if link.URL != "https://cdn.example.com/f?sign=1" {
		t.Fatalf("link url = %q", link.URL)
	}
	if link.Header.Get("User-Agent") != "test-ua" || link.Header.Get("Referer") != "https://pan.quark.cn" {
		t.Fatalf("unexpected link headers: %v", link.Header)
	}
}

var _ model.Obj = (*File)(nil)
