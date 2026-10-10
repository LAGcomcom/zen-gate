package subs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// singBoxReleaseAPI is SagerNet's latest-release feed; the asset we want is
// named sing-box-<ver>-windows-amd64.zip.
const singBoxReleaseAPI = "https://api.github.com/repos/SagerNet/sing-box/releases/latest"

// Manager owns the sing-box sidecar: subscription fetch/parse, config
// generation, child-process lifecycle, node health and the lane.Rotator
// implementation. One Manager per app.
type Manager struct {
	st  *store.Store
	log func(format string, args ...any)

	mu       sync.Mutex
	nodes    []Node
	health   map[string]*NodeHealth
	cmd      *exec.Cmd
	logFile  *os.File
	stopping bool
	rr       atomic.Uint64
	lastSync time.Time

	// sticky remembers (conversation → exit) so one conversation keeps the
	// same exit and a client's prompt cache has a chance to hit. It is a
	// preference, never a lock: an unusable exit is dropped immediately.
	sticky   map[string]string
	stickyAt map[string]int64

	// bans is the (exit, model) ledger: once an exit fails for a model, later
	// requests stop re-hitting the same wall. fails/failAt drive the adaptive
	// delay — a transient failure parks the pair for seconds, and only a
	// repeated one grows it. There is deliberately no mirror table of
	// "known-good" pairs: preferring one pins every concurrent request onto
	// the first success (measured in #23), so the ledger only ever removes
	// candidates.
	bans     map[string]time.Time
	fails    map[string]int
	failAt   map[string]time.Time
	lastErr  string
	version  string

	deathCancel context.CancelFunc // bounds the dead-node recheck loop
}

// NodeHealth is one node's live state for the rotation pool and the UI.
type NodeHealth struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Proto     string `json:"proto"`
	Port      int    `json:"port"`
	Alive     bool   `json:"alive"`
	IP        string `json:"ip,omitempty"`
	Country   string `json:"country,omitempty"`
	LatencyMs int    `json:"latencyMs,omitempty"`
	LastCheck int64  `json:"lastCheck,omitempty"`
	// LastOKMs is when a **real request** last succeeded through this node,
	// as opposed to when it was last probed. A probe is a sample and a served
	// request is proof, so this is what clears a cooldown / a ban.
	LastOKMs  int64 `json:"lastOkMs,omitempty"`
	CoolUntil int64 `json:"-"`
}

// NewManager builds the manager; logging goes through log (may be nil).
func NewManager(st *store.Store, log func(string, ...any)) *Manager {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Manager{
		st: st, log: log,
		health:   map[string]*NodeHealth{},
		sticky:   map[string]string{},
		bans:     map[string]time.Time{},
		fails:    map[string]int{},
		failAt:   map[string]time.Time{},
		stickyAt: map[string]int64{},
	}
}

// BinaryPath resolves the sing-box executable: the user-configured path wins,
// then the auto-downloaded copy under <home>/bin/sing-box.exe.
func (m *Manager) BinaryPath() string {
	if p := strings.TrimSpace(m.st.Config().SingBoxPath); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p := filepath.Join(store.Home(), "bin", "sing-box.exe")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// Download fetches the official sing-box windows-amd64 release zip via the
// lane's proxy-aware client and unpacks sing-box.exe into <home>/bin/.
func (m *Manager) Download(ctx context.Context) (string, error) {
	client := lane.Client()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, singBoxReleaseAPI, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("查询 sing-box 发布失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &rel); err != nil {
		return "", err
	}
	assetURL := ""
	for _, a := range rel.Assets {
		n := strings.ToLower(a.Name)
		if strings.Contains(n, "windows-amd64") && strings.HasSuffix(n, ".zip") {
			assetURL = a.URL
			break
		}
	}
	if assetURL == "" {
		return "", fmt.Errorf("发布 %s 中没有 windows-amd64.zip 资产", rel.TagName)
	}
	m.log("下载 sing-box %s …", rel.TagName)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	resp, err = client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载 sing-box 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("下载 sing-box 失败: HTTP %d", resp.StatusCode)
	}
	zbuf, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(zbuf), int64(len(zbuf)))
	if err != nil {
		return "", fmt.Errorf("解压 sing-box 失败: %w", err)
	}
	binDir := filepath.Join(store.Home(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(binDir, "sing-box.exe")
	for _, f := range zr.File {
		base := strings.ToLower(filepath.Base(f.Name))
		if base != "sing-box.exe" {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return "", err
		}
		dst, err := os.OpenFile(out+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			src.Close()
			return "", err
		}
		_, cerr := io.Copy(dst, src)
		src.Close()
		dst.Close()
		if cerr != nil {
			return "", cerr
		}
		if err := os.Rename(out+".tmp", out); err != nil {
			return "", err
		}
		m.log("sing-box 已就绪: %s", out)
		return out, nil
	}
	return "", fmt.Errorf("压缩包里没有 sing-box.exe")
}

// Refresh is the full cycle: fetch + parse every enabled subscription, write
// the sidecar config, (re)start the process and probe all nodes.
func (m *Manager) Refresh(ctx context.Context) error {
	cfg := m.st.Config()
	if !cfg.SubsEnabled {
		return fmt.Errorf("订阅轮询未启用")
	}
	var nodes []Node
	var errs []string
	client := lane.Client()
	for _, s := range cfg.Subscriptions {
		if !s.Enabled {
			continue
		}
		got, err := FetchSubscription(ctx, client, s.URL)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", displayName(s), err))
			continue
		}
		m.log("订阅 %s 解析出 %d 个节点", displayName(s), len(got))
		nodes = append(nodes, got...)
	}
	if len(nodes) == 0 {
		if len(errs) > 0 {
			return fmt.Errorf("所有订阅都失败了: %s", strings.Join(errs, "; "))
		}
		return fmt.Errorf("没有启用的订阅")
	}
	configPath, err := WriteConfig(nodes)
	if err != nil {
		return err
	}
	exe := m.BinaryPath()
	if exe == "" {
		return fmt.Errorf("没有 sing-box.exe——请在设置里填写路径或点「下载」")
	}
	if err := m.startProcess(exe, configPath, len(nodes)); err != nil {
		return err
	}
	m.mu.Lock()
	m.nodes = nodes
	m.health = map[string]*NodeHealth{}
	for i, n := range nodes {
		m.health[n.ID] = &NodeHealth{ID: n.ID, Name: n.Name, Proto: n.Proto, Port: PortBase + i, Alive: true}
	}
	m.lastSync = time.Now()
	m.lastErr = strings.Join(errs, "; ")
	m.mu.Unlock()
	if len(errs) > 0 {
		m.log("部分订阅失败: %s", strings.Join(errs, "; "))
	}
	// Probing the moment startProcess returns kills the whole pool: sing-box
	// has only just been exec'd, its inbounds are not listening yet, and one
	// round of guaranteed-failure probes flips every node to Alive=false.
	// Wait for the first socks port to accept before the health pass; the
	// grace covers the remaining outbounds loading.
	go func() {
		waitPortReady(PortBase, portDialWait)
		m.ProbeAll(context.Background())
		m.startDeathLoop()
	}()
	return nil
}

// portDialWait bounds one port-readiness wait: 20 seconds covers sing-box's
// cold start on slow disks; past that we probe anyway and let the death loop
// recover false negatives.
const portDialWait = 20 * time.Second

// deathProbeInterval is how often a dead node gets another look. It exists
// because ProbeAll's verdict is a sample: without this loop, nodes killed by
// one unlucky round wait for the next full refresh (manual, or the hourly
// one) while every client request fails on "没有健康节点".
const deathProbeInterval = 2 * time.Minute

// startDeathLoop re-probes dead nodes on a ticker for the process's life.
// A repeat call replaces the previous loop instead of stacking. The loop
// deliberately runs on its own context — binding it to a caller's timeout
// (issue #29 defect 3) made it silently vanish while the pool stayed dead.
func (m *Manager) startDeathLoop() {
	m.mu.Lock()
	if m.deathCancel != nil {
		m.deathCancel()
	}
	pctx, pcancel := context.WithCancel(context.Background())
	m.deathCancel = pcancel
	m.mu.Unlock()
	go func() {
		t := time.NewTicker(deathProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
				m.ProbeDead(pctx)
			}
		}
	}()
}

// waitPortReady blocks until sing-box's first socks inbound accepts a TCP
// connection (inbounds bind in config order, so the base port going up means
// the process finished booting), or until wait elapses.
func waitPortReady(port int, wait time.Duration) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			conn.Close()
			time.Sleep(2 * time.Second) // let the remaining outbounds bind
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func displayName(s store.Subscription) string {
	if s.Name != "" {
		return s.Name
	}
	return s.URL
}

// startProcess (re)launches sing-box; the supervisor goroutine restarts it on
// abnormal exit with capped backoff until Stop. nodeCount is how many inbounds
// the config binds — the restart guard has to cover all of them, so it is
// passed in rather than read from a field the caller has not published yet.
func (m *Manager) startProcess(exe, configPath string, nodeCount int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return fmt.Errorf("管理器已停止")
	}
	addrs := inboundPorts(nodeCount)
	if m.cmd != nil {
		_ = stopAndWait(m.cmd.Process, addrs, restartWait)
		m.cmd = nil
	}
	// A sidecar left by an earlier zen-gate owns these ports and nothing here
	// can signal it. Starting anyway would bind-fail, exit, and restart forever.
	if err := reapOrphan(addrs, restartWait); err != nil {
		return err
	}
	if err := checkConfig(exe, configPath); err != nil {
		return err
	}
	logPath := filepath.Join(store.SubsDir(), "sing-box.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "run", "-c", configPath, "-D", store.SubsDir())
	cmd.SysProcAttr = singBoxProcAttr()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("启动 sing-box 失败: %w", err)
	}
	// The kill job is what makes an orphan impossible from now on; the pid file
	// is how the next run recognises this one.
	if err := tieToParent(cmd.Process); err != nil {
		m.log("侧车没有并入回收作业: %v", err)
	}
	if err := writePidfileAt(cmd.Process.Pid, cmd.Path); err != nil {
		m.log("写入 sing-box.pid 失败: %v", err)
	}
	if m.logFile != nil {
		m.logFile.Close()
	}
	m.logFile, m.cmd = logf, cmd
	m.version = queryVersion(exe)
	// nodeCount, not len(m.nodes): Refresh publishes the node list only after
	// this returns, and the boot log printed "0 节点" even with 77 nodes bound.
	m.log("sing-box %s 已启动 (pid %d, %d 节点)", m.version, cmd.Process.Pid, nodeCount)
	go m.supervise(cmd)
	return nil
}

// restartWait bounds how long a restart waits for the old sidecar's sockets to
// come back. sing-box closes its listeners immediately on terminate, so this
// only has to cover the OS's own teardown.
const restartWait = 3 * time.Second

// checkConfig validates the generated config with `sing-box check` so a
// schema mistake surfaces as a message instead of a crash-looping child.
func checkConfig(exe, configPath string) error {
	out, err := exec.Command(exe, "check", "-c", configPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sing-box 配置校验失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func queryVersion(exe string) string {
	out, err := exec.Command(exe, "version").CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "version ") {
			return strings.TrimSpace(strings.TrimPrefix(strings.ToLower(line), "version "))
		}
	}
	return strings.TrimSpace(firstLine(string(out)))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (m *Manager) supervise(cmd *exec.Cmd) {
	err := cmd.Wait()
	m.mu.Lock()
	same := m.cmd == cmd
	stopping := m.stopping
	if same {
		m.cmd = nil
	}
	m.mu.Unlock()
	if !same || stopping {
		return
	}
	m.log("sing-box 意外退出(%v),5 秒后重启", err)
	time.Sleep(5 * time.Second)
	m.mu.Lock()
	exe, configPath := m.BinaryPath(), filepath.Join(store.SubsDir(), "sing-box.json")
	nodeCount := len(m.nodes)
	m.mu.Unlock()
	if exe == "" {
		return
	}
	_ = m.startProcess(exe, configPath, nodeCount)
}

// Stop kills the sidecar; called from every app shutdown path (the tray-quit
// path ends in os.Exit, so this must run before it).
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopping = true
	cmd := m.cmd
	nodeCount := len(m.nodes)
	m.cmd = nil
	if m.deathCancel != nil {
		m.deathCancel()
		m.deathCancel = nil
	}
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = stopAndWait(cmd.Process, inboundPorts(nodeCount), restartWait)
	}
	_ = removePidfile()
}

// Apply toggles the feature on: enable in config, run a full refresh cycle.
func (m *Manager) Apply(ctx context.Context) error {
	m.mu.Lock()
	m.stopping = false
	m.mu.Unlock()
	return m.Refresh(ctx)
}

// SetPath records a user-provided sing-box.exe path (empty clears it back to
// the auto-downloaded copy) and hot-restarts the sidecar if it is running.
func (m *Manager) SetPath(path string) error {
	path = strings.TrimSpace(path)
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("文件不存在: %s", path)
		}
	}
	m.mu.Lock()
	m.st.Mutate(func(cfg *store.Config) { cfg.SingBoxPath = path })
	_ = m.st.Save()
	wasRunning := m.cmd != nil
	nodeCount := len(m.nodes)
	m.mu.Unlock()
	if wasRunning {
		configPath := filepath.Join(store.SubsDir(), "sing-box.json")
		if _, err := os.Stat(configPath); err == nil {
			if exe := m.BinaryPath(); exe != "" {
				return m.startProcess(exe, configPath, nodeCount)
			}
		}
	}
	return nil
}

// Status summarizes the sidecar for the admin state payload.
func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := m.cmd != nil && m.cmd.Process != nil
	alive := 0
	nodes := []any{}
	for _, n := range m.nodes {
		h := m.health[n.ID]
		if h == nil {
			continue
		}
		if h.Alive {
			alive++
		}
		nodes = append(nodes, h)
	}
	return map[string]any{
		"running":  running,
		"version":  m.version,
		"nodes":    nodes,
		"alive":    alive,
		"lastSync": m.lastSync.Unix(),
		"lastErr":  m.lastErr,
		"portBase": PortBase,
	}
}

// Healthy implements lane.Rotator.
func (m *Manager) Healthy() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.health {
		if h.Alive && (h.CoolUntil == 0 || time.Now().UnixMilli() >= h.CoolUntil) {
			n++
		}
	}
	return n
}
