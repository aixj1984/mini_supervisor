package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"gopkg.in/natefinch/lumberjack.v2"
	"gopkg.in/yaml.v3"
)

type LogConfig struct {
	File       string `yaml:"file"`
	MaxSize    int    `yaml:"max_size"`
	MaxBackups int    `yaml:"max_backups"`
	MaxAge     int    `yaml:"max_age"`
	Compress   bool   `yaml:"compress"`
}

type Service struct {
	Name       string   `yaml:"name"`
	Cmd        []string `yaml:"cmd"`
	WorkDir    string   `yaml:"work_dir"`
	Auto       bool     `yaml:"auto"`
	RetryCount int      `yaml:"retry_count"`
}

type ServiceState struct {
	FailCount    int       `json:"fail_count"`
	IsRunning    bool      `json:"is_running"`
	PID          int       `json:"pid"`
	StartTime    time.Time `json:"start_time"`
	ExitTime     time.Time `json:"exit_time"`
	LastError    string    `json:"last_error"`
	LastFailTime time.Time `json:"last_fail_time"`
}

type Config struct {
	Log        LogConfig `yaml:"log"`
	Services   []Service `yaml:"services"`
	WebHookURL string    `yaml:"webhook_url"`
}

type ServiceManager struct {
	Configs map[string]*Service
	States  map[string]*ServiceState
	Procs   map[string]*exec.Cmd
	Mutex   sync.RWMutex // 改为读写锁
	WebHook string
}

// 设置日志
func setupLogger(cfg LogConfig) {
	if cfg.File != "" {
		log.SetOutput(&lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSize,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAge,
			Compress:   cfg.Compress,
		})
	}
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

// 加载配置
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	return &cfg, nil
}

// 发送通知
func sendNotify(webhookURL, appName, status, content string) {
	if webhookURL == "" {
		return
	}
	log.Printf("[通知] [%s] [%s]: %s\n", appName, status, content)
	// 异步发送通知，避免阻塞主流程
	go func() {
		notify := new(WebHook)
		notify.Send(webhookURL, appName, status, content)
	}()
}

// 准备命令执行环境
func (sm *ServiceManager) prepareCommand(svc *Service) (*exec.Cmd, error) {
	if len(svc.Cmd) == 0 {
		return nil, fmt.Errorf("命令不能为空")
	}

	cmd := exec.Command(svc.Cmd[0], svc.Cmd[1:]...)

	if svc.WorkDir != "" {
		if _, err := os.Stat(svc.WorkDir); os.IsNotExist(err) {
			return nil, fmt.Errorf("工作目录不存在: %s", svc.WorkDir)
		}
		cmd.Dir = svc.WorkDir
	}

	// 设置进程组，便于进程管理
	// cmd.SysProcAttr = &syscall.SysProcAttr{
	// 	HideWindow: true,
	// }

	return cmd, nil
}

// 计算重试延迟（指数退避）
func (sm *ServiceManager) calculateRetryDelay(failCount int) time.Duration {
	baseDelay := 3 * time.Second
	maxDelay := 5 * time.Minute

	delay := baseDelay * time.Duration(1<<(failCount-1)) // 使用位运算替代 math.Pow
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// 注册进程
func (sm *ServiceManager) registerProcess(name string, cmd *exec.Cmd) {
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()

	sm.Procs[name] = cmd
	state := sm.States[name]
	state.PID = cmd.Process.Pid
	state.StartTime = time.Now()
	state.IsRunning = true
	// 注意：不要在这里重置 FailCount，只有在正常退出或手动启动时才重置
	state.LastError = ""
}

// 核心服务运行方法
func (sm *ServiceManager) TryRunService(name string) {
	// 使用读锁快速检查
	sm.Mutex.RLock()
	svc, ok := sm.Configs[name]
	state := sm.States[name]
	if !ok {
		sm.Mutex.RUnlock()
		log.Printf("服务 [%s] 未找到\n", name)
		return
	}

	// 检查失败次数（需要写锁）
	sm.Mutex.RUnlock()
	sm.Mutex.Lock()
	if state.FailCount >= svc.RetryCount {
		sm.Mutex.Unlock()
		log.Printf("服务 [%s] 超过失败上限，暂停自动重启\n", name)
		return
	}
	sm.Mutex.Unlock()

	// 准备命令
	cmd, err := sm.prepareCommand(svc)
	if err != nil {
		sm.handleStartError(name, err, "")
		return
	}

	// 设置输出管道用于实时日志
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sm.handleStartError(name, fmt.Errorf("创建输出管道失败: %w", err), "")
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		sm.handleStartError(name, fmt.Errorf("创建错误管道失败: %w", err), "")
		return
	}

	// 启动命令
	if err := cmd.Start(); err != nil {
		sm.handleStartError(name, err, "")
		return
	}

	// 注册进程
	sm.registerProcess(name, cmd)
	sendNotify(sm.WebHook, name, "service running", "服务启动成功")

	// 启动输出监控
	go sm.monitorOutput(name, stdout, stderr)
	// 启动退出监控
	go sm.waitForExit(name, cmd)
}

// 监控命令输出 - 简化版本
func (sm *ServiceManager) monitorOutput(name string, stdout, stderr io.Reader) {
	// 监控标准输出 - 完全忽略正常输出
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			// 正常输出完全忽略，不记录日志也不输出到控制台
			// 如果需要查看实时输出，可以取消下面的注释
			// fmt.Printf("[%s] %s\n", name, scanner.Text())
		}
	}()

	// 监控错误输出 - 只记录错误信息
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				log.Printf("[%s ERROR] %s", name, line)
			}
		}
	}()
}

// 处理进程退出
func (sm *ServiceManager) handleProcessExit(name string, cmd *exec.Cmd, exitErr error) {
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()

	// 清理进程引用
	delete(sm.Procs, name)

	state := sm.States[name]
	state.IsRunning = false
	state.ExitTime = time.Now()

	if exitErr != nil {
		log.Printf("服务 [%s] 异常退出: %v\n", name, exitErr)
		state.LastError = exitErr.Error()
		state.FailCount++

		// 检查是否需要重启
		svc := sm.Configs[name]
		if state.FailCount < svc.RetryCount {
			log.Printf("服务 [%s] 将在3秒后重启 (失败次数: %d/%d)\n", name, state.FailCount, svc.RetryCount)
			sendNotify(sm.WebHook, name, "service exited",
				fmt.Sprintf("异常退出: %v (失败次数: %d/%d)", exitErr, state.FailCount, svc.RetryCount))
			time.AfterFunc(3*time.Second, func() { sm.TryRunService(name) })
		} else {
			log.Printf("服务 [%s] 超过重试次数 (%d/%d)，停止重启\n", name, state.FailCount, svc.RetryCount)
			sendNotify(sm.WebHook, name, "service stopped",
				fmt.Sprintf("超过重试次数限制 (%d/%d)", state.FailCount, svc.RetryCount))
		}
	} else {
		log.Printf("服务 [%s] 正常退出\n", name)
		// 正常退出时重置失败计数
		state.FailCount = 0
		state.LastError = ""
		sendNotify(sm.WebHook, name, "service exited", "正常退出")
	}
}

// 等待进程退出
func (sm *ServiceManager) waitForExit(name string, cmd *exec.Cmd) {
	err := cmd.Wait()
	sm.handleProcessExit(name, cmd, err)
}

// 启动服务
func (sm *ServiceManager) StartService(name string) error {
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()

	if _, ok := sm.Configs[name]; !ok {
		return fmt.Errorf("服务 [%s] 未找到", name)
	}

	// 重置状态（手动启动时重置失败计数）
	state := sm.States[name]
	state.FailCount = 0
	state.LastError = ""

	go sm.TryRunService(name)
	return nil
}

// 停止服务
func (sm *ServiceManager) StopService(name string) error {
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()

	cmd, ok := sm.Procs[name]
	if !ok || cmd.Process == nil {
		return fmt.Errorf("服务 [%s] 未运行", name)
	}

	// 使用进程组终止整个进程树
	// 先尝试发送中断信号（跨平台更可用），失败时直接 Kill
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("终止服务失败: %w", err)
		}
	}

	delete(sm.Procs, name)
	state := sm.States[name]
	state.IsRunning = false
	state.ExitTime = time.Now()

	log.Printf("服务 [%s] 已停止\n", name)
	return nil
}

// 重启服务
func (sm *ServiceManager) RestartService(name string) error {
	if err := sm.StopService(name); err != nil {
		log.Printf("停止服务 [%s] 失败: %v", name, err)
		// 继续尝试启动
	}

	time.Sleep(1 * time.Second)
	return sm.StartService(name)
}

// API 处理函数
func (sm *ServiceManager) ListServices(w http.ResponseWriter, r *http.Request) {
	sm.Mutex.RLock()
	defer sm.Mutex.RUnlock()

	result := make(map[string]interface{})
	for name, svc := range sm.Configs {
		state := sm.States[name]
		status := "stopped"
		if state.IsRunning {
			status = "running"
		}

		result[name] = map[string]interface{}{
			"status":          status,
			"fail_count":      state.FailCount,
			"pid":             state.PID,
			"start_time":      state.StartTime,
			"last_error":      state.LastError,
			"auto_restart":    svc.Auto,
			"retry_count":     svc.RetryCount,
			"retry_remaining": svc.RetryCount - state.FailCount,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (sm *ServiceManager) GetServiceStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	sm.Mutex.RLock()
	defer sm.Mutex.RUnlock()

	svc, exists := sm.Configs[name]
	if !exists {
		http.Error(w, "服务未找到", http.StatusNotFound)
		return
	}

	state := sm.States[name]
	status := "stopped"
	if state.IsRunning {
		status = "running"
	}

	resp := map[string]interface{}{
		"name":            name,
		"status":          status,
		"fail_count":      state.FailCount,
		"pid":             state.PID,
		"start_time":      state.StartTime,
		"last_error":      state.LastError,
		"auto_restart":    svc.Auto,
		"retry_count":     svc.RetryCount,
		"retry_remaining": svc.RetryCount - state.FailCount,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (sm *ServiceManager) HandleAction(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]
	action := vars["action"]

	var err error
	switch action {
	case "start":
		err = sm.StartService(name)
	case "stop":
		err = sm.StopService(name)
	case "restart":
		err = sm.RestartService(name)
	default:
		http.Error(w, "不支持的操作", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// 清理函数
func (sm *ServiceManager) Cleanup() {
	log.Println("清理子进程...")
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()

	for name, cmd := range sm.Procs {
		if cmd.Process != nil {
			log.Printf("终止服务 [%s] (PID=%d)\n", name, cmd.Process.Pid)
			// 使用进程组终止
			// 先尝试发送中断信号（跨平台更可用），失败时直接 Kill
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				_ = cmd.Process.Kill()
			}
		}
	}
}

// 处理启动错误
func (sm *ServiceManager) handleStartError(name string, err error, stderr string) {
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()

	state := sm.States[name]
	state.FailCount++
	state.LastError = err.Error()
	state.LastFailTime = time.Now()

	log.Printf("服务 [%s] 启动失败 (失败次数: %d): %v\n", name, state.FailCount, err)
	if stderr != "" {
		log.Printf("服务 [%s] 错误输出: %s\n", name, stderr)
	}

	// 发送通知
	message := fmt.Sprintf("启动失败: %s (失败次数: %d)", err.Error(), state.FailCount)
	sendNotify(sm.WebHook, name, "launch error", message)

	// 检查是否需要重试
	svc := sm.Configs[name]
	if state.FailCount < svc.RetryCount {
		retryDelay := sm.calculateRetryDelay(state.FailCount)
		log.Printf("服务 [%s] 将在 %v 后重试 (%d/%d)\n", name, retryDelay, state.FailCount, svc.RetryCount)
		time.AfterFunc(retryDelay, func() { sm.TryRunService(name) })
	} else {
		log.Printf("服务 [%s] 超过失败上限 (%d/%d)，暂停自动重启\n", name, state.FailCount, svc.RetryCount)
		sendNotify(sm.WebHook, name, "service stopped",
			fmt.Sprintf("超过失败上限 (%d/%d)，暂停自动重启", state.FailCount, svc.RetryCount))
	}
}

// 注册路由
func registerRoutes(sm *ServiceManager) {
	r := mux.NewRouter()
	r.HandleFunc("/services", sm.ListServices).Methods("GET")
	r.HandleFunc("/services/{name}/status", sm.GetServiceStatus).Methods("GET")
	r.HandleFunc("/services/{name}/{action}", sm.HandleAction).Methods("POST")

	// 添加健康检查端点
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	}).Methods("GET")

	log.Println("API 服务器启动在 :8080")
	if err := http.ListenAndServe(":8080", r); err != nil {
		log.Fatalf("API 服务器启动失败: %v", err)
	}
}

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	setupLogger(cfg.Log)
	log.Println("服务守护进程启动...")

	manager := &ServiceManager{
		Configs: make(map[string]*Service),
		States:  make(map[string]*ServiceState),
		Procs:   make(map[string]*exec.Cmd),
		WebHook: cfg.WebHookURL,
	}

	// 初始化服务
	for _, svc := range cfg.Services {
		// 重试次数默认值
		if svc.RetryCount <= 0 {
			svc.RetryCount = 3
		}
		// 使用指针，避免拷贝
		manager.Configs[svc.Name] = &svc
		manager.States[svc.Name] = &ServiceState{}

		if svc.Auto {
			log.Printf("自动启动服务: %s", svc.Name)
			go manager.TryRunService(svc.Name)
		}
	}

	// 信号处理
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("接收到信号: %v", sig)
		manager.Cleanup()
		os.Exit(0)
	}()

	// 启动 API 服务器
	go registerRoutes(manager)

	log.Println("服务守护进程运行中...")
	select {} // 保持主进程运行
}
