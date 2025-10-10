package main

import (
	"encoding/json" //diy
	"flag"
	"fmt"
	"io" //diy
	"net/http" //diy
	"net/url" //diy
	"os"
	"sort"
	"time"

	"github.com/faceair/clash-speedtest/speedtester"
	"github.com/metacubex/mihomo/log"
	"github.com/olekukonko/tablewriter"
	"github.com/schollz/progressbar/v3"
	"gopkg.in/yaml.v3"
)

var (
	configPathsConfig = flag.String("c", "", "config file path, also support http(s) url")
	filterRegexConfig = flag.String("f", ".+", "filter proxies by name, use regexp")
	serverURL         = flag.String("server-url", "https://speed.cloudflare.com", "server url")
	downloadSize      = flag.Int("download-size", 50*1024*1024, "download size for testing proxies")
	uploadSize        = flag.Int("upload-size", 20*1024*1024, "upload size for testing proxies")
	timeout           = flag.Duration("timeout", time.Second*5, "timeout for testing proxies")
	concurrent        = flag.Int("concurrent", 4, "download concurrent size")
	outputPath        = flag.String("output", "", "output config file path")
	maxLatency        = flag.Duration("max-latency", 800*time.Millisecond, "filter latency greater than this value")
	minSpeed          = flag.Float64("min-speed", 5, "filter speed less than this value(unit: MB/s)")
	noticeURL         = flag.String("notice-url", "", "通知url，前面是各种参数，通知内容放在最后") //diy
	speedtestURL      = flag.String("speedtest-url", "", "图表url，前面是各种参数，通知内容放在最后") //diy
	testCount        = flag.Int("testcount", 1, "每个节点测试次数 默认1次")
	maxRetries        = flag.Int("maxretries", 5, "不通后的最大重试次数 默认5次")
	fastMode          = flag.Bool("fast", false, "快速测试模式，仅测试节点延迟") //diy
	debug             = flag.Bool("debug", false, "显示测试输出，进度条等信息") //diy
)

const (
	colorRed    = "" //"\033[31m" //diy 太傻了
	colorGreen  = "" //"\033[32m"
	colorYellow = "" //"\033[33m"
	colorReset  = "" //"\033[0m"
)

var failureCount int //diy 失败次数
var lastFailedTime time.Time //diy 最后一次失败时间
var httpClient = &http.Client{ //diy HTTP客户端作为全局变量，避免每次都创建新的客户端
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns: 100,
		IdleConnTimeout: 90 * time.Second,
	},
}

func main() {
	flag.Parse()
	log.SetLevel(log.SILENT)

	if *configPathsConfig == "" {
		log.Fatalln("please specify the configuration file")
	}

	speedTester := speedtester.New(&speedtester.Config{
		ConfigPaths:  *configPathsConfig,
		FilterRegex:  *filterRegexConfig,
		ServerURL:    *serverURL,
		DownloadSize: *downloadSize,
		UploadSize:   *uploadSize,
		Timeout:      *timeout,
		Concurrent:   *concurrent,
		TestCount:    *testCount, //diy
		MaxRetries:    *maxRetries, //diy
		FastMode:     *fastMode, //diy 发送到 speedtester.go 的函数
		Debug:     *debug, //diy
	})

	//diy 循环检测
	for {
		allProxies, err := speedTester.LoadProxies()
		if err != nil {
			log.Fatalln("load proxies failed: %v", err)
		}
		//bar := progressbar.Default(int64(len(allProxies)), "测试中...") //diy 修复进度条显示错乱，设置进度条合适的宽度
		var bar *progressbar.ProgressBar
		if *debug { //diy
			bar = progressbar.NewOptions(int(len(allProxies)),
				progressbar.OptionSetWidth(10),                // 设置进度条的宽度
				progressbar.OptionShowIts(),                   // 显示百分比
			)
		}
		results := make([]*speedtester.Result, 0)
		speedTester.TestProxies(allProxies, func(result *speedtester.Result) {
			if *debug { //diy
				bar.Add(1)
				bar.Describe(result.ProxyName)
			}
			results = append(results, result)
		})

		sort.Slice(results, func(i, j int) bool {
			return results[i].DownloadSpeed > results[j].DownloadSpeed
		})

		if *debug { //diy
			printResults(results)
		}

		if *outputPath != "" {
			err = saveConfig(results)
			if err != nil {
				log.Fatalln("save config file failed: %v", err)
			}
			fmt.Printf("\nsave config file to: %s\n", *outputPath)
		}
		
		//发送部分 // 创建一个存储节点信息的结构体
		var nodesData []map[string]string
		// 收集失败的节点（根据故障标准）
		var failedNodes []string
		for _, result := range results {
			if result.Latency == 0 || result.PacketLoss == 100 {
				failedNodes = append(failedNodes, result.ProxyName)
			}
			//发送部分
			var port string
			if p, ok := result.ProxyConfig["port"].(int); ok {
				port = fmt.Sprintf("%d", p) // 转换成字符串
			}
			nodesData = append(nodesData, map[string]string{
				"port":      port,
				"latency":   result.FormatLatency(),
				//"node_name": result.ProxyName,
			})
		}
		//发送部分 // 将节点信息转为JSON
		jsonData, err := json.Marshal(nodesData)
		if err != nil {
			fmt.Printf("failed to marshal JSON data: %v", err)
		}
		// 调用函数发送 HTTP 请求
		if *speedtestURL != "" {
			if err := sendHttpRequest(*speedtestURL, string(jsonData)); err != nil {
				fmt.Println("发送HTTP请求错误:", err)
			} else {
				if *debug {
					fmt.Println("消息成功发送")
				}
			}
		}
		// 如果节点失败，发送消息 %0A 是换行
		if len(failedNodes) > 0 {
			content := fmt.Sprintf("节点监控 %d 个:", len(failedNodes))
			for _, node := range failedNodes {
				content += node + " "
			}

			// 计算下一次允许发送的时间
			var requiredInterval time.Duration
			if failureCount >= 6 {
				requiredInterval = 1 * time.Hour
				content = "缓1时 " + content
			} else {
				requiredInterval = 10 * time.Minute
				if failureCount > 0 { // 第一条消息不加前缀，后续消息加"缓10分"
					content = "缓10分 " + content
				}
			}
			// 检查是否达到发送间隔要求
			if lastFailedTime.IsZero() || time.Since(lastFailedTime) >= requiredInterval { //IsZero检查是否首次发送
				// 调用函数发送 HTTP 请求
				if *noticeURL != "" {
					if err := sendHttpRequest(*noticeURL, content); err != nil {
						fmt.Println("发送HTTP请求错误:", err)
					} else {
						if *debug {
							fmt.Println("消息成功发送")
						}
						// 更新发送时间和计数
						lastFailedTime = time.Now()
						failureCount++
					}
				}
			}
			if *debug {
				fmt.Println("最后失败时间 ", lastFailedTime)
				fmt.Println("失败计数:", failureCount)
			}
		} else {
			// 无失败节点时重置计数器
			failureCount = 0
			lastFailedTime = time.Time{} // 重置时间
			if *debug {
				fmt.Println("全部正常")
			}
		}
		if *debug {
			fmt.Println("当前时间 ", time.Now())
			fmt.Println("累计失败次数 ", failureCount)
			fmt.Println("休息5分钟,准备再次检测...")
		}
		time.Sleep(time.Minute * 5) // 等待下一个间隔 (5 分钟)
	}
}

//将失败的节点列表发送消息
func sendHttpRequest(urlinfo string, data string) error {
	//流程：把URL的get转为post参数发送，避免 Cloudflare 对 URL 的最大长度限制
	// 解析完整 URL
	parsedURL, err := url.Parse(urlinfo)
	if err != nil {
		return fmt.Errorf("URL解析失败: %v", err)
	}

	// 获取基础 URL（不包含查询参数的部分）
	baseURL := fmt.Sprintf("%s://%s%s", parsedURL.Scheme, parsedURL.Host, parsedURL.Path)

	// 获取所有查询参数
	queryParams := parsedURL.Query()
	
	// 遍历查询参数并检查哪个参数为空 就用为空的参数名称作为消息name发送
	for param, values := range queryParams {
		// 如果值为空（即切片为空），则输出该参数名
		if values[0] == "" {
			// 添加 name和发送参数
			//fmt.Println("参数名为空:", param)
			queryParams.Set(param, data)
		}
	}

	// 将查询参数转换为 POST 表单数据
	formData := url.Values{}
	for key, values := range queryParams {
		if len(values) > 0 {
			formData.Set(key, values[0])
		}
	}

	// 发送 POST 请求
	resp, err := httpClient.PostForm(baseURL, formData)
	if err != nil {
		return fmt.Errorf("未能发送请求: %v", err)
	}
	defer resp.Body.Close()

	// 检查响应状态
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 请求失败 状态: %v", resp.Status)
	}

	// 可以根据需要读取响应
	if *debug {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("获取响应失败: %v", err)
		}
		fmt.Println(string(body))
	}

	return nil
}

func printResults(results []*speedtester.Result) {
	table := tablewriter.NewWriter(os.Stdout)

	table.SetHeader([]string{
		"序号",
		"节点名称",
		"类型",
		"延迟",
		"抖动",
		"丢包率",
		"下载速度",
		"上传速度",
	})

	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("\t")
	table.SetNoWhiteSpace(true)

	for i, result := range results {
		idStr := fmt.Sprintf("%d.", i+1)

		// 延迟颜色
		latencyStr := result.FormatLatency()
		if result.Latency > 0 {
			if result.Latency < 800*time.Millisecond {
				latencyStr = colorGreen + latencyStr + colorReset
			} else if result.Latency < 1500*time.Millisecond {
				latencyStr = colorYellow + latencyStr + colorReset
			} else {
				latencyStr = colorRed + latencyStr + colorReset
			}
		} else {
			latencyStr = colorRed + latencyStr + colorReset
		}

		jitterStr := result.FormatJitter()
		if result.Jitter > 0 {
			if result.Jitter < 800*time.Millisecond {
				jitterStr = colorGreen + jitterStr + colorReset
			} else if result.Jitter < 1500*time.Millisecond {
				jitterStr = colorYellow + jitterStr + colorReset
			} else {
				jitterStr = colorRed + jitterStr + colorReset
			}
		} else {
			jitterStr = colorRed + jitterStr + colorReset
		}

		// 丢包率颜色
		packetLossStr := result.FormatPacketLoss()
		if result.PacketLoss < 10 {
			packetLossStr = colorGreen + packetLossStr + colorReset
		} else if result.PacketLoss < 20 {
			packetLossStr = colorYellow + packetLossStr + colorReset
		} else {
			packetLossStr = colorRed + packetLossStr + colorReset
		}

		// 下载速度颜色 (以MB/s为单位判断)
		downloadSpeed := result.DownloadSpeed / (1024 * 1024)
		downloadSpeedStr := result.FormatDownloadSpeed()
		if downloadSpeed >= 10 {
			downloadSpeedStr = colorGreen + downloadSpeedStr + colorReset
		} else if downloadSpeed >= 5 {
			downloadSpeedStr = colorYellow + downloadSpeedStr + colorReset
		} else {
			downloadSpeedStr = colorRed + downloadSpeedStr + colorReset
		}

		// 上传速度颜色
		uploadSpeed := result.UploadSpeed / (1024 * 1024)
		uploadSpeedStr := result.FormatUploadSpeed()
		if uploadSpeed >= 5 {
			uploadSpeedStr = colorGreen + uploadSpeedStr + colorReset
		} else if uploadSpeed >= 2 {
			uploadSpeedStr = colorYellow + uploadSpeedStr + colorReset
		} else {
			uploadSpeedStr = colorRed + uploadSpeedStr + colorReset
		}

		row := []string{
			idStr,
			result.ProxyName,
			result.ProxyType,
			latencyStr,
			jitterStr,
			packetLossStr,
			downloadSpeedStr,
			uploadSpeedStr,
		}

		table.Append(row)
	}

	fmt.Println()
	table.Render()
	fmt.Println()
}

func saveConfig(results []*speedtester.Result) error {
	filteredResults := make([]*speedtester.Result, 0)
	for _, result := range results {
		if *maxLatency > 0 && result.Latency > *maxLatency {
			continue
		}
		if *minSpeed > 0 && float64(result.DownloadSpeed)/(1024*1024) < *minSpeed {
			continue
		}
		filteredResults = append(filteredResults, result)
	}

	proxies := make([]map[string]any, 0)
	for _, result := range filteredResults {
		proxies = append(proxies, result.ProxyConfig)
	}

	config := &speedtester.RawConfig{
		Proxies: proxies,
	}
	yamlData, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(*outputPath, yamlData, 0o644)
}
