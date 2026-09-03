package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultAPIBase = "https://api.github.com"

var errNotFound = errors.New("不存在")

type apiError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("GitHub API %s %s → %d：%s", e.Method, e.Path, e.Status, msg)
}

func (e *apiError) Is(target error) bool {
	return target == errNotFound && e.Status == 404
}

// retryable 报告这个错误值不值得重试（网络抖动、限流、5xx）。
func (e *apiError) retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

// api 是一个只覆盖本项目用到的那几个接口的 GitHub REST 客户端。
type api struct {
	token string
	base  string
	http  *http.Client
}

func newAPI(token string) *api {
	return &api{
		token: token,
		base:  defaultAPIBase,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *api) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		hdr, err := a.once(ctx, method, path, body, out)
		if err == nil {
			return hdr, nil
		}
		var ae *apiError
		if errors.As(err, &ae) && !ae.retryable() {
			return hdr, err
		}
		if ctx.Err() != nil {
			return hdr, err
		}
		last = err
	}
	return nil, last
}

func (a *api) once(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", appName)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s %s：%w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 400 {
		var payload struct{ Message string }
		_ = json.Unmarshal(raw, &payload)
		msg := payload.Message
		switch {
		case resp.StatusCode == 401:
			msg = "token 无效或已过期（" + msg + "）"
		case resp.StatusCode == 403 && strings.Contains(strings.ToLower(msg), "rate limit"):
			msg = "触发限流（" + msg + "）"
		case resp.StatusCode == 403:
			msg = msg + "（很可能是 token 缺少 codespace 或 repo 权限）"
		}
		return resp.Header, &apiError{Status: resp.StatusCode, Method: method, Path: path, Message: msg}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.Header, fmt.Errorf("解析 %s %s 的响应：%w", method, path, err)
		}
	}
	return resp.Header, nil
}

// ---------- 数据结构：只留用得上的字段 ----------

type codespace struct {
	Name             string `json:"name"`
	DisplayName      string `json:"display_name"`
	State            string `json:"state"`
	LastUsedAt       string `json:"last_used_at"`
	CreatedAt        string `json:"created_at"`
	WebURL           string `json:"web_url"`
	PendingOperation bool   `json:"pending_operation"`
	Repository       struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// running 报告这个 codespace 现在就能连。
func (c *codespace) running() bool { return c.State == "Available" }

// stopped 报告它需要先开机。
func (c *codespace) stopped() bool { return c.State == "Shutdown" || c.State == "Archived" }

type repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

func (a *api) currentUser(ctx context.Context) (string, error) {
	info, err := a.whoami(ctx)
	return info.Login, err
}

// userInfo 是启动自检需要的东西：token 属于谁、有哪些 scope。
// Scopes 来自 X-OAuth-Scopes 头，只有经典 PAT（ghp_…）会返回，
// fine-grained token 是空字符串——那就只能等真正调用时才知道权限够不够。
type userInfo struct {
	Login  string
	Scopes string
}

func (a *api) whoami(ctx context.Context) (userInfo, error) {
	var user struct{ Login string }
	hdr, err := a.do(ctx, "GET", "/user", nil, &user)
	if err != nil {
		return userInfo{}, err
	}
	scopes := ""
	if hdr != nil {
		scopes = hdr.Get("X-OAuth-Scopes")
	}
	return userInfo{Login: user.Login, Scopes: scopes}, nil
}

// hasScope 判断 scope 列表里有没有某一项（GitHub 返回的是 "repo, codespace" 这种）。
func hasScope(scopes, want string) bool {
	for _, s := range strings.Split(scopes, ",") {
		if strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}

func (a *api) listCodespaces(ctx context.Context) ([]codespace, error) {
	var payload struct {
		Codespaces []codespace `json:"codespaces"`
	}
	if _, err := a.do(ctx, "GET", "/user/codespaces?per_page=100", nil, &payload); err != nil {
		return nil, err
	}
	return payload.Codespaces, nil
}

func (a *api) getCodespace(ctx context.Context, name string) (*codespace, error) {
	var cs codespace
	if _, err := a.do(ctx, "GET", "/user/codespaces/"+url.PathEscape(name), nil, &cs); err != nil {
		return nil, err
	}
	return &cs, nil
}

// startCodespace 是幂等的：已经在跑的 codespace 不算错误。
func (a *api) startCodespace(ctx context.Context, name string) error {
	_, err := a.do(ctx, "POST", "/user/codespaces/"+url.PathEscape(name)+"/start", nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 409 {
		return nil
	}
	return err
}

// createCodespace 只传必需的字段：机器规格、地区、休眠时间全部交给 GitHub 的默认值，
// 这样 idle/停机策略完全按你 GitHub 账号里的设置走，网关不掺和。
func (a *api) createCodespace(ctx context.Context, repoID int64, displayName string) (*codespace, error) {
	body := map[string]any{
		"repository_id": repoID,
		"display_name":  displayName,
	}
	var cs codespace
	if _, err := a.do(ctx, "POST", "/user/codespaces", body, &cs); err != nil {
		return nil, err
	}
	if cs.Name == "" {
		return nil, errors.New("GitHub 没有返回新建 codespace 的名字")
	}
	return &cs, nil
}

func (a *api) getRepo(ctx context.Context, nwo string) (*repository, error) {
	owner, name, ok := strings.Cut(nwo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("仓库 %q 必须是 owner/name 的形式", nwo)
	}
	var repo repository
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if _, err := a.do(ctx, "GET", path, nil, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// createPublicRepo 建一个公开仓库当 codespace 的载体。auto_init 让它带一个初始
// 提交（有默认分支才能建 codespace）。
func (a *api) createPublicRepo(ctx context.Context, name string) (*repository, error) {
	body := map[string]any{
		"name":        name,
		"private":     false,
		"auto_init":   true,
		"description": "Dev box for csgw (ssh root@codespace)",
	}
	var repo repository
	if _, err := a.do(ctx, "POST", "/user/repos", body, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// pollInterval 是轮询 codespace 状态的间隔（测试里会调小）。
var pollInterval = 3 * time.Second
