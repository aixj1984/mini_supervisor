package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	"mini_supervisor/httpclient"
)

type WebHookCfg struct {
	URL      string `json:"url"`
	Template string `json:"template"`
}

// WebHook
type WebHook struct{}

// Send 发送
func (webHook *WebHook) Send(webHookURL, appName, status, content string) {
	// webHookSetting.Url = "https://open.feishu.cn/open-apis/bot/v2/hook/967762b9-d655-4772-b424-2a0d2ba06961"
	params := make(map[string]interface{})
	params["TaskName"] = appName
	params["Status"] = status
	params["Result"] = content

	notifyTemplate := `{
		"msg_type": "post",
		"content": {
			"post": {
				"zh_cn": {
					"title": "服务启停通知",
					"content": [
						[
							{
								"tag": "text",
								"text": "%s"
							}
						]
					]
				}
			}
		}
	}`

	// msg.Name = EscapeJSON(msg.Name)
	// msg.Output = EscapeJSON(msg.Output)
	msg := parseNotifyTemplate("服务名称: {{.TaskName}}\n服务状态: {{.Status}}\n运行结果: {{.Result}} ", params)

	urls := strings.Split(webHookURL, ";")
	for _, rawURL := range urls {
		// 解析URL
		parsedURL, err := url.Parse(rawURL)
		if err != nil {
			log.Printf("解析URL出错:%s", err)
			continue
		}
		// 获取规范化后的URL字符串
		normalizedURL := parsedURL.String()

		webHook.send(fmt.Sprintf(notifyTemplate, EscapeJSON(msg)), normalizedURL)
	}
}

func parseNotifyTemplate(notifyTemplate string, params map[string]interface{}) string {
	tmpl, err := template.New("notify").Parse(notifyTemplate)
	if err != nil {
		return fmt.Sprintf("解析通知模板失败: %s", err.Error())
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, params)
	if err != nil {
		return fmt.Sprintf("通知模板更新失败: %s", err.Error())
	}
	return buf.String()
}

// EscapeJSON 转义json特殊字符
func EscapeJSON(s string) string {
	specialChars := []string{"\\", "\b", "\f", "\n", "\r", "\t", "\""}
	replaceChars := []string{"\\\\", "\\b", "\\f", "\\n", "\\r", "\\t", "\\\""}

	return ReplaceStrings(s, specialChars, replaceChars)
}

// ReplaceStrings 批量替换字符串
func ReplaceStrings(s string, old []string, replace []string) string {
	if s == "" {
		return s
	}
	if len(old) != len(replace) {
		return s
	}

	for i, v := range old {
		s = strings.Replace(s, v, replace[i], 1000)
	}

	return s
}

func (webHook *WebHook) send(content string, url string) {
	timeout := 30
	maxTimes := 3
	i := 0
	for i < maxTimes {
		resp := httpclient.PostJSON(url, content, timeout)
		if resp.StatusCode == http.StatusOK {
			break
		}
		i++
		time.Sleep(2 * time.Second)
		if i < maxTimes {
			log.Printf("webHook#发送消息失败#%s#消息内容-%s", resp.Body, content)
		}
	}
}
