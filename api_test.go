package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub 是一个够用的假 GitHub：只实现本项目用到的那几个接口，
// 但走的是真实的 HTTP + 真实的 REST 客户端代码路径。
type fakeGitHub struct {
	mu sync.Mutex

	login      string
	codespaces map[string]*codespace
	repos      map[string]int64

	// 状态机：get 到第 n 次时把状态推进到 Available
	provisionGets int

	createdCodespaces int
	createdRepos      int
	starts            int
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		login:      "octocat",
		codespaces: map[string]*codespace{},
		repos:      map[string]int64{},
	}
}

func (f *fakeGitHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitHub) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/user" && r.Method == "GET":
		json.NewEncoder(w).Encode(map[string]string{"login": f.login})

	case path == "/user/codespaces" && r.Method == "GET":
		list := make([]codespace, 0, len(f.codespaces))
		for _, cs := range f.codespaces {
			list = append(list, *cs)
		}
		json.NewEncoder(w).Encode(map[string]any{"codespaces": list})

	case path == "/user/codespaces" && r.Method == "POST":
		var body struct {
			RepositoryID int64  `json:"repository_id"`
			DisplayName  string `json:"display_name"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.createdCodespaces++
		name := fmt.Sprintf("fake-codespace-%d", f.createdCodespaces)
		cs := &codespace{Name: name, DisplayName: body.DisplayName, State: "Provisioning"}
		cs.Repository.ID = body.RepositoryID
		cs.Repository.FullName = "octocat/codespace-box"
		f.codespaces[name] = cs
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(cs)

	case strings.HasSuffix(path, "/start") && r.Method == "POST":
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/user/codespaces/"), "/start")
		cs, ok := f.codespaces[name]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.starts++
		cs.State = "Starting"
		json.NewEncoder(w).Encode(cs)

	case strings.HasPrefix(path, "/user/codespaces/") && r.Method == "GET":
		name := strings.TrimPrefix(path, "/user/codespaces/")
		cs, ok := f.codespaces[name]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if cs.State != "Available" {
			f.provisionGets++
			if f.provisionGets >= 2 { // 两次轮询之后就绪，测试才跑得快
				cs.State = "Available"
			}
		}
		json.NewEncoder(w).Encode(cs)

	case path == "/user/repos" && r.Method == "POST":
		var body struct {
			Name    string `json:"name"`
			Private bool   `json:"private"`
			Init    bool   `json:"auto_init"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Private {
			http.Error(w, `{"message":"test expects a public repo"}`, http.StatusBadRequest)
			return
		}
		if !body.Init {
			http.Error(w, `{"message":"test expects auto_init"}`, http.StatusBadRequest)
			return
		}
		f.createdRepos++
		full := f.login + "/" + body.Name
		f.repos[full] = 4242
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(repository{ID: 4242, FullName: full})

	case strings.HasPrefix(path, "/repos/") && r.Method == "GET":
		full := strings.TrimPrefix(path, "/repos/")
		id, ok := f.repos[full]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(repository{ID: id, FullName: full})

	default:
		io.WriteString(w, "{}")
		http.Error(w, `{"message":"unexpected `+r.Method+" "+path+`"}`, http.StatusNotFound)
	}
}

func testProvider(t *testing.T, f *fakeGitHub) (*codespacesProvider, *Config) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := f.start(t)
	pollInterval = time.Millisecond
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Token = "test-token"
	a := newAPI("test-token")
	a.base = srv.URL
	return &codespacesProvider{cfg: cfg, api: a, log: log.New(io.Discard, "", 0)}, cfg
}

func silent(string) {}

// 账号里一个 codespace 都没有：应该建一个公开仓库 + 一个 codespace，然后等它就绪。
func TestEnsureCreatesRepoAndCodespace(t *testing.T) {
	f := newFakeGitHub()
	p, cfg := testProvider(t, f)

	id, err := p.Ensure(context.Background(), silent)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if id != "fake-codespace-1" {
		t.Fatalf("id = %q", id)
	}
	if f.createdRepos != 1 || f.createdCodespaces != 1 {
		t.Fatalf("建了 %d 个仓库 / %d 个 codespace，各应该是 1 个", f.createdRepos, f.createdCodespaces)
	}
	if f.starts != 0 {
		t.Fatalf("新建的 codespace 不该再调 start，实际调了 %d 次", f.starts)
	}
	if cfg.Codespace != id {
		t.Fatalf("没有记住 codespace 名字：%q", cfg.Codespace)
	}
	if cfg.Repo != "octocat/codespace-box" {
		t.Fatalf("没有记住来源仓库：%q", cfg.Repo)
	}

	// 第二次（绕过 warm 缓存）应该直接复用，不再创建任何东西。
	p.warmID, p.warmAt = "", time.Time{}
	if _, err := p.Ensure(context.Background(), silent); err != nil {
		t.Fatal(err)
	}
	if f.createdCodespaces != 1 {
		t.Fatalf("复用失败，又建了一个：%d", f.createdCodespaces)
	}
}

// 记住的 codespace 是停止状态：应该 start 一次并等到 Available。
func TestEnsureStartsStoppedCodespace(t *testing.T) {
	f := newFakeGitHub()
	f.codespaces["kept"] = &codespace{Name: "kept", DisplayName: displayMarker, State: "Shutdown"}
	p, cfg := testProvider(t, f)
	cfg.Codespace = "kept"

	id, err := p.Ensure(context.Background(), silent)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if id != "kept" {
		t.Fatalf("id = %q", id)
	}
	if f.starts != 1 {
		t.Fatalf("start 调了 %d 次，应该是 1 次", f.starts)
	}
	if f.createdCodespaces != 0 {
		t.Fatal("不该创建新的 codespace")
	}
}

// 记住的名字已经不存在，但账号里恰好只有一个：直接认领它，不新建。
func TestEnsureAdoptsTheOnlyCodespace(t *testing.T) {
	f := newFakeGitHub()
	f.codespaces["leftover"] = &codespace{Name: "leftover", State: "Available"}
	p, cfg := testProvider(t, f)
	cfg.Codespace = "deleted-one"

	id, err := p.Ensure(context.Background(), silent)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if id != "leftover" {
		t.Fatalf("id = %q，应该认领已有的那个", id)
	}
	if f.createdCodespaces != 0 || f.createdRepos != 0 {
		t.Fatal("不该创建任何东西")
	}
}

// token 无效时报错要说得清楚，而且不能把 token 打出来。
func TestEnsureReportsBadToken(t *testing.T) {
	f := newFakeGitHub()
	p, _ := testProvider(t, f)
	p.api.token = "wrong-token"

	_, err := p.Ensure(context.Background(), silent)
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "token 无效") {
		t.Fatalf("错误信息不好懂：%v", err)
	}
	if strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("错误信息里泄漏了 token：%v", err)
	}
}
