package server

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed panel.html
var panelHTMLRaw string

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/admin" && r.URL.Path != "/panel" && r.URL.Path != "/panel/" {
		http.NotFound(w, r)
		return
	}
	html := panelHTMLRaw
	html = strings.ReplaceAll(html, "__SERVICE_NAME__", "codearts2api")
	html = strings.ReplaceAll(html, "__SERVICE_TITLE__", "CodeArts2API")
	html = strings.ReplaceAll(html, "__LOGO__", "CA")
	html = strings.ReplaceAll(html, "__ACCENT__", "#0284c7")
	// CodeArts 无积分/签到；「签到」隐藏文案改为保活相关
	html = strings.ReplaceAll(html, ">全员签到<", ">全员保活<")
	html = strings.ReplaceAll(html, ">拉积分<", ">刷新状态<")
	html = strings.ReplaceAll(html, ">签到<", ">保活<")
	html = strings.ReplaceAll(html, ">积分<", ">状态<")
	html = strings.ReplaceAll(html, "积分合计", "账号健康")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}
