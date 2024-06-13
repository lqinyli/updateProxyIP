package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-ping/ping"
)

type Config struct {
	Email       string     `json:"email"`
	Key         string     `json:"key"`
	DomainInfos [][]string `json:"domainInfos"`
}

type LatencyResult struct {
	Latency  int     `json:"latency"`
	IP       string  `json:"ip"`
	LossRate float64 `json:"lossRate"`
}

func getLatency(ip string, count int) LatencyResult {
	var latency int = 9999
	var lossRate float64 = 1.00
	var lossRateStr string

	pinger, err := ping.NewPinger(ip)
	if err != nil {
		log.Printf("ping 错误: %v", err)
		return LatencyResult{Latency: 9999, IP: ip, LossRate: 1.00}
	}
	os := runtime.GOOS
	if os == "windows" {
		pinger.SetPrivileged(true)
	}
	pinger.Count = count
	pinger.Timeout = 1000 * 1000 * 1000 // 1秒
	pinger.Run()
	stats := pinger.Statistics()
	if stats.PacketLoss < 35 {
		latency = int(stats.AvgRtt.Seconds() * 1000)
		lossRateStr = fmt.Sprintf("%.2f", stats.PacketLoss)
		lossRate, _ = strconv.ParseFloat(lossRateStr, 64)
		return LatencyResult{Latency: latency, IP: ip, LossRate: lossRate}
	} else {
		return LatencyResult{Latency: latency, IP: ip, LossRate: lossRate}
	}
}

func getIP(content string) string {
	var wg sync.WaitGroup
	ipList := strings.Split(strings.Trim(content, "\n"), "\n")
	resultChan := make(chan LatencyResult, len(ipList))

	for _, ip := range ipList {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			resultChan <- getLatency(ip, 4)
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	var results []LatencyResult
	for result := range resultChan {
		results = append(results, result)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Latency < results[j].Latency
	})
	nowtime := time.Now().Format("2006-01-02 15:04:05")
	for _, item := range results {
		if item.LossRate <= 0.35 {
			lossRateStr := fmt.Sprintf("%.2f", item.LossRate)
			log.Printf("[%s]所选ip-%s的丢包率为：%s，延时为：%dms\n", nowtime, item.IP, lossRateStr, item.Latency)
			return item.IP
		}
	}
	return ""
}

func uploadIP(ip, name string, domain string, email string, key string) {
	retry := 0
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?name=%s", domain)
	header := map[string]string{
		"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/94.0.4606.54 Safari/537.36",
		"Content-Type": "application/json",
		"X-Auth-Email": email,
		"X-Auth-Key":   key,
	}

	client := &http.Client{Timeout: 15 * time.Second}
	for retry < 5 {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			log.Printf("第%v次访问 %v 失败: %v", retry, url, err)
			retry++
			continue
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("第%v次创建 %v 访问出错: %v", retry, url, err)
			retry++
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("第%v次读取 %v 访问结果出错: %v", retry, url, err)
			retry++
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		var result map[string]interface{}
		json.Unmarshal(body, &result)
		zid := result["result"].([]interface{})[0].(map[string]interface{})["id"].(string)

		url = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s.%s", zid, name, domain)
		req, err = http.NewRequest("GET", url, nil)
		if err != nil {
			log.Printf("第%v次访问 %v 失败: %v", retry, url, err)
			retry++
			continue
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}

		resp, err = client.Do(req)
		if err != nil {
			log.Printf("第%v次创建 %v 访问出错: %v", retry, url, err)
			retry++
			continue
		}

		body, err = io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("第%v次读取 %v 访问结果出错: %v", retry, url, err)
			retry++
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		var dnsRecords map[string]interface{}
		json.Unmarshal(body, &dnsRecords)
		resultList := dnsRecords["result"].([]interface{})
		rid := ""
		for _, record := range resultList {
			if record.(map[string]interface{})["type"].(string) == "A" {
				rid = record.(map[string]interface{})["id"].(string)
				break
			}
		}

		params := map[string]interface{}{
			"id":      zid,
			"type":    "A",
			"name":    fmt.Sprintf("%s.%s", name, domain),
			"content": ip,
		}
		if rid == "" {
			continue
		}

		url = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zid, rid)
		data, _ := json.Marshal(params)
		req, err = http.NewRequest("PUT", url, bytes.NewBuffer(data))
		if err != nil {
			log.Printf("第%v次访问 %v 失败: %v", retry, url, err)
			retry++
			continue
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}

		resp, err = client.Do(req)
		if err != nil {
			log.Printf("第%v次创建 %v 访问出错: %v", retry, url, err)
			retry++
			continue
		}

		if resp.StatusCode == 200 {
			nowtime := time.Now().Format("2006-01-02 15:04:05")
			log.Printf("[%s]成功更新%s.%s的ip为%s\n", nowtime, name, domain, ip)
			break
		} else {
			retry++
		}
	}
	if retry >= 5 {
		nowtime := time.Now().Format("2006/01/02-15:04:05")
		log.Printf("[%s]%s.%s的ip更新失败\n", nowtime, name, domain)
	}
}

func handleMain(config Config, domainInfo []string) {
	nowtime := time.Now().Format("2006-01-02 15:04:05")
	url := fmt.Sprintf("%s.%s", domainInfo[0], domainInfo[1])
	result := getLatency(url, 10)
	if result.Latency > 200 {
		resp, err := http.Get("https://zip.baipiao.eu.org")
		if err != nil {
			log.Printf("下载ZIP文件错误: %v", err)
			return
		}
		defer resp.Body.Close()
		buf, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("读取ZIP文件错误: %v", err)
			return
		}

		z, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
		if err != nil {
			log.Printf("打开ZIP文件错误: %v", err)
			return
		}

		for _, file := range z.File {
			var content []byte

			if len(domainInfo) == 3 && strings.Contains(file.Name, domainInfo[2]) {
				rc, err := file.Open()
				if err != nil {
					log.Printf("无法打开ZIP中的文件: %v", err)
					return
				}
				content, err = io.ReadAll(rc) // 赋值 content
				rc.Close()
				if err != nil {
					log.Printf("无法读取ZIP中的文件: %v", err)
					return
				}
			} else {
				rc, err := file.Open()
				if err != nil {
					log.Printf("无法打开ZIP中的文件: %v", err)
					return
				}
				content, err = io.ReadAll(rc) // 赋值 content
				rc.Close()
				if err != nil {
					log.Printf("无法读取ZIP中的文件: %v", err)
					return
				}
			}
			ip := getIP(string(content))
			if ip != "" {
				uploadIP(ip, domainInfo[0], domainInfo[1], config.Email, config.Key)
				break
			}
		}
	} else {
		log.Printf("[%s]域名%s的ip延时为%dms小于200ms，未更新\n", nowtime, url, result.Latency)
	}

}

func main() {
	// 定义命令行参数
	filePath := flag.String("file", "config.json", "文件路径和名称")

	// 解析命令行参数
	flag.Parse()

	// 打开文件
	file, err := os.Open(*filePath)
	if err != nil {
		log.Printf("无法打开配置文件: %v\n", err)
	}
	defer file.Close()

	// 读取文件内容
	bytes, err := io.ReadAll(file)
	if err != nil {
		log.Printf("无法读取配置文件: %v\n", err)
	}

	// 解析 JSON 文件内容
	var config Config
	if err := json.Unmarshal(bytes, &config); err != nil {
		log.Printf("无法解析配置文件: %v\n", err)
	}

	for _, domainInfo := range config.DomainInfos {
		handleMain(config, domainInfo)
	}
}
