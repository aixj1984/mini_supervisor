package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	Name    string   `yaml:"name"`
	Cmd     []string `yaml:"cmd"`
	WorkDir string   `yaml:"work_dir"`
	Auto    bool     `yaml:"auto"`
}

type ServiceState struct {
	FailCount       int
	NotificationOff bool
}

type Config struct {
	Log        LogConfig `yaml:"log"`
	Services   []Service `yaml:"services"`
	WebHookURL string    `yaml:"webhook_url"`
}

type ServiceManager struct {
	Configs map[string]Service
	States  map[string]*ServiceState
	Procs   map[string]*exec.Cmd
	Mutex   sync.Mutex
	WebHook string
}

func setupLogger(cfg LogConfig) {
	log.SetOutput(&lumberjack.Logger{
		Filename:   cfg.File,
		MaxSize:    cfg.MaxSize,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAge,
		Compress:   cfg.Compress,
	})
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func sendNotify(webhookURL, appName, status, content string) {
	if webhookURL == "" {
		return
	}
	log.Printf("[通知] [%s] [%s]: %s\n", appName, status, content)
	notify := new(WebHook)
	notify.Send(webhookURL, appName, status, content)
}

func (sm *ServiceManager) TryRunService(name string) {
	sm.Mutex.Lock()
	svc, ok := sm.Configs[name]
	state := sm.States[name]
	if !ok {
		sm.Mutex.Unlock()
		log.Printf("服务 [%s] 未找到\n", name)
		return
	}
	if state.FailCount >= 3 {
		sm.Mutex.Unlock()
		log.Printf("服务 [%s] 超过失败上限，暂停自动重启\n", name)
		return
	}
	sm.Mutex.Unlock()

	cmd := exec.Command(svc.Cmd[0], svc.Cmd[1:]...)
	if svc.WorkDir != "" {
		cmd.Dir = svc.WorkDir
	}
	cmd.Stdout = os.Stdout
	// var outBuf bytes.Buffer
	// 	// cmd.Stdout = &outBuf
	// cmd.Stderr = os.Stderr
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	err := cmd.Start()
	if err != nil {
		sm.Mutex.Lock()
		state.FailCount++
		sm.Mutex.Unlock()

		log.Printf("服务 [%s] 启动失败: %v\n", name, err)
		log.Printf("执行路径: %s, 工作目录: %s\n", cmd.Path, cmd.Dir)
		sendNotify(sm.WebHook, name, fmt.Sprintf("launch error：%d times", state.FailCount), err.Error())
		time.AfterFunc(3*time.Second, func() { sm.TryRunService(name) })
		return
	}

	sm.Mutex.Lock()
	sm.Procs[name] = cmd
	sm.Mutex.Unlock()
	sendNotify(sm.WebHook, name, "service running", "ok")

	go sm.waitForExit(name, cmd, &errBuf)
}

func (sm *ServiceManager) waitForExit(name string, cmd *exec.Cmd, errBuf *bytes.Buffer) {
	err := cmd.Wait()
	sm.Mutex.Lock()
	delete(sm.Procs, name)
	state := sm.States[name]
	state.FailCount++
	sm.Mutex.Unlock()
	if err != nil {
		log.Printf("服务 [%s] 退出: %s\n", name, err.Error())
		log.Printf("服务 [%s] 错误输出:\n%s\n", name, errBuf.String())
		sendNotify(sm.WebHook, name, fmt.Sprintf("service exit：%d times", state.FailCount), err.Error())
	} else {
		sendNotify(sm.WebHook, name, fmt.Sprintf("service exit：%d times", state.FailCount), "正常退出")
	}

	if state.FailCount <= 3 {
		time.AfterFunc(2*time.Second, func() {
			sm.TryRunService(name)
		})
	} else {
		log.Printf("服务 [%s] 达到失败上限，不再自动拉起\n", name)
	}
}

func (sm *ServiceManager) StartService(name string) error {
	sm.Mutex.Lock()
	state := sm.States[name]
	state.FailCount = 0
	sm.Mutex.Unlock()
	sm.TryRunService(name)
	return nil
}

func (sm *ServiceManager) StopService(name string) error {
	sm.Mutex.Lock()
	cmd, ok := sm.Procs[name]
	if !ok || cmd.Process == nil {
		sm.Mutex.Unlock()
		return fmt.Errorf("服务 [%s] 未运行", name)
	}
	err := cmd.Process.Kill()
	delete(sm.Procs, name)
	sm.Mutex.Unlock()
	return err
}

func (sm *ServiceManager) RestartService(name string) error {
	_ = sm.StopService(name)
	time.Sleep(1 * time.Second)
	sm.Mutex.Lock()
	sm.States[name].FailCount = 0
	sm.Mutex.Unlock()
	sm.TryRunService(name)
	return nil
}

func (sm *ServiceManager) ListServices(w http.ResponseWriter, r *http.Request) {
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()
	result := make(map[string]interface{})
	for name := range sm.Configs {
		status := "stopped"
		if cmd, ok := sm.Procs[name]; ok && cmd.Process != nil {
			status = "running"
		}
		result[name] = map[string]interface{}{
			"status":     status,
			"fail_count": sm.States[name].FailCount,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (sm *ServiceManager) GetServiceStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()
	_, exists := sm.Configs[name]
	if !exists {
		http.Error(w, "服务未找到", 404)
		return
	}
	status := "stopped"
	if cmd, ok := sm.Procs[name]; ok && cmd.Process != nil {
		status = "running"
	}
	resp := map[string]interface{}{
		"name":       name,
		"status":     status,
		"fail_count": sm.States[name].FailCount,
	}
	json.NewEncoder(w).Encode(resp)
}

func (sm *ServiceManager) Cleanup() {
	log.Println("清理子进程...")
	sm.Mutex.Lock()
	defer sm.Mutex.Unlock()
	for name, cmd := range sm.Procs {
		if cmd.Process != nil {
			log.Printf("终止服务 [%s] (PID=%d)\n", name, cmd.Process.Pid)
			_ = cmd.Process.Kill()
		}
	}
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
		http.Error(w, "不支持的操作", 400)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
	} else {
		w.Write([]byte("ok"))
	}
}

func registerRoutes(sm *ServiceManager) {
	r := mux.NewRouter()
	r.HandleFunc("/services", sm.ListServices).Methods("GET")
	r.HandleFunc("/services/{name}/status", sm.GetServiceStatus).Methods("GET")
	r.HandleFunc("/services/{name}/{action}", sm.HandleAction).Methods("POST")
	http.ListenAndServe(":8080", r)
}

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	setupLogger(cfg.Log)
	log.Println("服务守护进程启动...")

	manager := &ServiceManager{
		Configs: make(map[string]Service),
		States:  make(map[string]*ServiceState),
		Procs:   make(map[string]*exec.Cmd),
		WebHook: cfg.WebHookURL,
	}

	// 捕捉 Ctrl+C / kill 信号
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		manager.Cleanup()
		os.Exit(0)
	}()

	for _, svc := range cfg.Services {
		manager.Configs[svc.Name] = svc
		manager.States[svc.Name] = &ServiceState{}
		if svc.Auto {
			go manager.TryRunService(svc.Name)
		}
	}

	go registerRoutes(manager)

	select {} // 保持运行
}
