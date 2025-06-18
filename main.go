package main

import (
	"log"
	"os"
	"os/exec"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"gopkg.in/yaml.v3"
)

type LogConfig struct {
	File       string `yaml:"file"`
	MaxSize    int    `yaml:"max_size"` // MB
	MaxBackups int    `yaml:"max_backups"`
	MaxAge     int    `yaml:"max_age"` // days
	Compress   bool   `yaml:"compress"`
}

type Service struct {
	Name    string   `yaml:"name"`
	Cmd     []string `yaml:"cmd"`
	WorkDir string   `yaml:"work_dir"` // 可选字段
}

type ServiceState struct {
	FailCount       int  // 连续失败次数
	NotificationOff bool // 是否暂停通知
}

type Config struct {
	Log        LogConfig `yaml:"log"`
	Services   []Service `yaml:"services"`
	WebHookURL string    `yaml:"webhook_url"`
}

func setupLogger(cfg LogConfig) {
	log.SetOutput(&lumberjack.Logger{
		Filename:   cfg.File,
		MaxSize:    cfg.MaxSize, // MB
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAge, // days
		Compress:   cfg.Compress,
	})
	log.SetFlags(log.LstdFlags | log.Lshortfile) // [时间] + [文件:行号]
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
	if len(webhookURL) == 0 {
		return
	}
	notify := new(WebHook)
	notify.Send(webhookURL, appName, status, content)
}

func runService(svc Service, webhookURL string, state *ServiceState) {
	for {
		log.Printf("启动服务 [%s]: %v\n", svc.Name, svc.Cmd)

		cmd := exec.Command(svc.Cmd[0], svc.Cmd[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if len(svc.WorkDir) > 0 {
			cmd.Dir = svc.WorkDir
		}

		err := cmd.Start()
		if err != nil {
			log.Printf("服务 [%s] 启动失败: %v\n", svc.Name, err)
			state.FailCount++
			if state.FailCount <= 3 {
				sendNotify(webhookURL, svc.Name, "launch error", err.Error())
			} else {
				log.Printf("服务 [%s] 通知暂停，因失败次数已达 %d\n", svc.Name, state.FailCount)
			}
			time.Sleep(3 * time.Second)
			continue
		}

		// 服务成功启动
		sendNotify(webhookURL, svc.Name, "service running", "ok")

		err = cmd.Wait()
		log.Printf("服务 [%s] 退出: %v\n", svc.Name, err)
		state.FailCount++
		if state.FailCount <= 3 {
			sendNotify(webhookURL, svc.Name, "service exit", err.Error())
		} else if !state.NotificationOff {
			log.Printf("服务 [%s] 通知暂停，因失败次数已达 %d\n", svc.Name, state.FailCount)
		}

		time.Sleep(2 * time.Second)
	}
}

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	setupLogger(cfg.Log)

	log.Println("启动日志服务...")

	states := make(map[string]*ServiceState)
	for _, svc := range cfg.Services {
		states[svc.Name] = &ServiceState{}
		go runService(svc, cfg.WebHookURL, states[svc.Name])
	}

	select {} // 保持运行
}
