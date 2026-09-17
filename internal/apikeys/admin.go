// ═══ 更新日志 ═══
// 2026-09-16：增加仅通过本机 Unix socket 访问的密钥管理接口，避免把管理能力暴露给普通调用密钥。
// 2026-09-17：管理接口支持模型绑定字段，与密钥库校验保持一致。
// 2026-09-17：拆分"路径被活着的进程占用"这一种失败，并支持等旧进程释放后重绑，
//
//	供热更新时新实例接手管理 socket（旧版本不会传 FD）。
package apikeys

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (s *Store) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ok": true, "keys": s.List(), "max_keys": MaxKeys})
	})
	mux.HandleFunc("POST /keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name   string   `json:"name"`
			Note   string   `json:"note"`
			Models []string `json:"models"`
		}
		if !readBody(w, r, &body) {
			return
		}
		entry, key, err := s.Create(body.Name, body.Note, body.Models)
		if err != nil {
			replyError(w, err)
			return
		}
		reply(w, 201, map[string]any{"ok": true, "entry": entry, "key": key})
	})
	mux.HandleFunc("PATCH /keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name    *string   `json:"name"`
			Note    *string   `json:"note"`
			Enabled *bool     `json:"enabled"`
			Models  *[]string `json:"models"`
		}
		if !readBody(w, r, &body) {
			return
		}
		if body.Name == nil && body.Note == nil && body.Enabled == nil && body.Models == nil {
			reply(w, 400, map[string]any{"ok": false, "message": "没有要修改的字段"})
			return
		}
		entry, err := s.Update(r.PathValue("id"), body.Name, body.Note, body.Enabled, body.Models)
		if err != nil {
			replyError(w, err)
			return
		}
		reply(w, 200, map[string]any{"ok": true, "entry": entry})
	})
	mux.HandleFunc("DELETE /keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Delete(r.PathValue("id")); err != nil {
			replyError(w, err)
			return
		}
		reply(w, 200, map[string]any{"ok": true})
	})
	return mux
}

func readBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(out)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if err != nil {
		reply(w, 400, map[string]any{"ok": false, "message": "请输入有效的密钥信息（请求体最多 8 KiB）"})
		return false
	}
	return true
}

func replyError(w http.ResponseWriter, err error) {
	code := 500
	message := "保存密钥失败，请检查网关日志"
	switch {
	case errors.Is(err, ErrNotFound):
		code = 404
		message = err.Error()
	case errors.Is(err, ErrInvalid):
		code = 400
		message = err.Error()
	case errors.Is(err, ErrInvalidModels):
		code = 400
		message = err.Error()
	case errors.Is(err, ErrLimit):
		code = 409
		message = err.Error()
	default:
		log.Printf("[api-keys] operation failed: %v", err)
	}
	reply(w, code, map[string]any{"ok": false, "message": message})
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// ErrSocketBusy 目标路径上已有正在服务的 socket（通常是上一个进程还没退出）。
var ErrSocketBusy = errors.New("API key socket is already in use")

func ListenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("API key socket path is not a socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, 300*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, ErrSocketBusy
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

// WaitUnix 在 deadline 内轮询绑定 Unix socket。
//
// 热更新时新实例先于旧实例退出就启动，旧实例（v1.3.1 及更早）不会把管理 socket 交出来，
// 只能等它收尾关闭监听、路径被释放后再绑。不能用同步等待替代：旧实例要等新实例报告
// 就绪才开始收尾，同步等会直接卡到超时。
func WaitUnix(path string, deadline, interval time.Duration) (net.Listener, error) {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	end := time.Now().Add(deadline)
	for {
		listener, err := ListenUnix(path)
		if err == nil {
			return listener, nil
		}
		if !errors.Is(err, ErrSocketBusy) {
			return nil, err
		}
		if time.Now().After(end) {
			return nil, fmt.Errorf("admin socket still busy after %s: %w", deadline, err)
		}
		time.Sleep(interval)
	}
}
