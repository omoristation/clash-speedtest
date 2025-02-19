package speedtester

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/provider"
	"github.com/metacubex/mihomo/constant"
	//"github.com/metacubex/mihomo/log" //diy
	"gopkg.in/yaml.v3"
)

type Config struct {
	ConfigPaths  string
	FilterRegex  string
	ServerURL    string
	DownloadSize int
	UploadSize   int
	Timeout      time.Duration
	Concurrent   int
	FastMode     bool //diy
}

type SpeedTester struct {
	config *Config
}

func New(config *Config) *SpeedTester {
	if config.Concurrent <= 0 {
		config.Concurrent = 1
	}
	if config.DownloadSize <= 0 {
		config.DownloadSize = 100 * 1024 * 1024
	}
	if config.UploadSize <= 0 {
		config.UploadSize = 10 * 1024 * 1024
	}
	return &SpeedTester{
		config: config,
	}
}

type CProxy struct {
	constant.Proxy
	Config map[string]any
}

type RawConfig struct {
	Providers map[string]map[string]any `yaml:"proxy-providers"`
	Proxies   []map[string]any          `yaml:"proxies"`
}

func (st *SpeedTester) LoadProxies() (map[string]*CProxy, error) {
	allProxies := make(map[string]*CProxy)
	//1.如果配置同时存在http和yaml，并且yaml的修改时间小于1小时,就使用yaml,如果大于1小时,就下载HTTP的保存为YAML的本地路径，再使用这个本地YAML配置 2.如果只存在HTTP或者YAML配置，就单独使用这个配置
	//diy 分离HTTP和YAML配置路径
	var httpConfig, yamlConfig string
	configs := strings.Split(st.config.ConfigPaths, ",")
	for _, cfg := range configs {
		if strings.HasPrefix(cfg, "http") {
			httpConfig = cfg
		} else {
			yamlConfig = cfg
		}
	}
	
	// 确定使用哪个配置
	configPath := ""
	if httpConfig != "" && yamlConfig != "" {
		// 两种配置都存在时的逻辑
		fileInfo, err := os.Stat(yamlConfig) //只获取文件的元信息（metadata）执行速度快，因为不需要读取文件内容
		if err == nil {
			modTime := fileInfo.ModTime()
			if time.Since(modTime) < time.Hour {
				// YAML修改时间小于1小时,使用YAML
				configPath = yamlConfig
			} else {
				// YAML修改时间大于1小时,下载HTTP配置
				if err := downloadConfig(httpConfig, yamlConfig); err != nil {
					return nil, fmt.Errorf("failed to download config: %w", err)
				}
				configPath = yamlConfig
			}
		} else {
			// YAML不存在,下载HTTP配置
			if err := downloadConfig(httpConfig, yamlConfig); err != nil {
				return nil, fmt.Errorf("failed to download config: %w", err)
			}
			configPath = yamlConfig
		}
	} else if httpConfig != "" {
		// 只有HTTP配置
		configPath = httpConfig
	} else if yamlConfig != "" {
		// 只有YAML配置
		configPath = yamlConfig
	} else {
		return nil, fmt.Errorf("no valid config path provided")
	}
	
	// 读取配置内容
	var body []byte
	var err error
	if strings.HasPrefix(configPath, "http") {
		resp, err := http.Get(configPath) //diy
		if err != nil {
			return nil, fmt.Errorf("failed to fetch config: %w", err) //diy
		}
		defer resp.Body.Close() //diy
		body, err = io.ReadAll(resp.Body)
	} else {
		body, err = os.ReadFile(configPath)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err) //diy
	}

	// 解析配置
	rawCfg := &RawConfig{
		Proxies: []map[string]any{},
	}
	if err := yaml.Unmarshal(body, rawCfg); err != nil {
		return nil, err
	}

	// 处理代理配置
	proxies := make(map[string]*CProxy)
	proxiesConfig := rawCfg.Proxies
	providersConfig := rawCfg.Providers

	for i, config := range proxiesConfig {
		proxy, err := adapter.ParseProxy(config)
		if err != nil {
			return nil, fmt.Errorf("proxy %d: %w", i, err)
		}
		if _, exist := proxies[proxy.Name()]; exist {
			return nil, fmt.Errorf("proxy %s is the duplicate name", proxy.Name())
		}
		proxies[proxy.Name()] = &CProxy{Proxy: proxy, Config: config}
	}

	// 处理provider配置
	for name, config := range providersConfig {
		if name == provider.ReservedName {
			return nil, fmt.Errorf("can not defined a provider called `%s`", provider.ReservedName)
		}
		pd, err := provider.ParseProxyProvider(name, config)
		if err != nil {
			return nil, fmt.Errorf("parse proxy provider %s error: %w", name, err)
		}
		if err := pd.Initial(); err != nil {
			return nil, fmt.Errorf("initial proxy provider %s error: %w", pd.Name(), err)
		}
		for _, proxy := range pd.Proxies() {
			proxies[fmt.Sprintf("[%s] %s", name, proxy.Name())] = &CProxy{Proxy: proxy}
		}
	}

	// 过滤代理
	for k, p := range proxies {
		switch p.Type() {
		case constant.Shadowsocks, constant.ShadowsocksR, constant.Snell, constant.Socks5, constant.Http,
			constant.Vmess, constant.Vless, constant.Trojan, constant.Hysteria, constant.Hysteria2,
			constant.WireGuard, constant.Tuic, constant.Ssh:
		default:
			continue
		}
		if _, ok := allProxies[k]; !ok {
			allProxies[k] = p
		}
	}

	// 应用正则过滤
	filterRegexp := regexp.MustCompile(st.config.FilterRegex)
	filteredProxies := make(map[string]*CProxy)
	for name := range allProxies {
		if filterRegexp.MatchString(name) {
			filteredProxies[name] = allProxies[name]
		}
	}
	return filteredProxies, nil
}
//diy 辅助函数:下载配置并保存到本地
func downloadConfig(httpURL, savePath string) error {
	resp, err := http.Get(httpURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	
	return os.WriteFile(savePath, body, 0644)
}

func (st *SpeedTester) TestProxies(proxies map[string]*CProxy, fn func(result *Result)) {
	for name, proxy := range proxies {
		fn(st.testProxy(name, proxy))
	}
}

type testJob struct {
	name  string
	proxy *CProxy
}

type Result struct {
	ProxyName     string         `json:"proxy_name"`
	ProxyType     string         `json:"proxy_type"`
	ProxyConfig   map[string]any `json:"proxy_config"`
	Latency       time.Duration  `json:"latency"`
	Jitter        time.Duration  `json:"jitter"`
	PacketLoss    float64        `json:"packet_loss"`
	DownloadSize  float64        `json:"download_size"`
	DownloadTime  time.Duration  `json:"download_time"`
	DownloadSpeed float64        `json:"download_speed"`
	UploadSize    float64        `json:"upload_size"`
	UploadTime    time.Duration  `json:"upload_time"`
	UploadSpeed   float64        `json:"upload_speed"`
}

func (r *Result) FormatDownloadSpeed() string {
	return formatSpeed(r.DownloadSpeed)
}

func (r *Result) FormatLatency() string {
	if r.Latency == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%dms", r.Latency.Milliseconds())
}

func (r *Result) FormatJitter() string {
	if r.Jitter == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%dms", r.Jitter.Milliseconds())
}

func (r *Result) FormatPacketLoss() string {
	return fmt.Sprintf("%.1f%%", r.PacketLoss)
}

func (r *Result) FormatUploadSpeed() string {
	return formatSpeed(r.UploadSpeed)
}

func formatSpeed(bytesPerSecond float64) string {
	units := []string{"B/s", "KB/s", "MB/s", "GB/s", "TB/s"}
	unit := 0
	speed := bytesPerSecond
	for speed >= 1024 && unit < len(units)-1 {
		speed /= 1024
		unit++
	}
	return fmt.Sprintf("%.2f%s", speed, units[unit])
}

func (st *SpeedTester) testProxy(name string, proxy *CProxy) *Result {
	result := &Result{
		ProxyName:   name,
		ProxyType:   proxy.Type().String(),
		ProxyConfig: proxy.Config,
	}

	// 1. 首先进行延迟测试
	latencyResult := st.testLatency(proxy)
	result.Latency = latencyResult.avgLatency

	// 如果是快速模式，只测试延迟，不测试抖动和丢包率 diy
	if !st.config.FastMode {
		result.Jitter = latencyResult.jitter
		result.PacketLoss = latencyResult.packetLoss
	}
	// 如果是快速模式，只测试延迟，直接返回结果 diy
	if st.config.FastMode {
		return result
	}

	// 如果延迟测试完全失败，直接返回
	if result.PacketLoss == 100 {
		return result
	}

	// 2. 并发进行下载和上传测试
	var wg sync.WaitGroup
	downloadResults := make(chan *downloadResult, st.config.Concurrent)

	// 计算每个并发连接的数据大小
	downloadChunkSize := st.config.DownloadSize / st.config.Concurrent
	uploadChunkSize := st.config.UploadSize / st.config.Concurrent

	// 启动下载测试
	for i := 0; i < st.config.Concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			downloadResults <- st.testDownload(proxy, downloadChunkSize)
		}()
	}
	wg.Wait()

	uploadResults := make(chan *downloadResult, st.config.Concurrent)

	// 启动上传测试
	for i := 0; i < st.config.Concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uploadResults <- st.testUpload(proxy, uploadChunkSize)
		}()
	}
	wg.Wait()

	// 3. 汇总结果
	var totalDownloadBytes, totalUploadBytes int64
	var totalDownloadTime, totalUploadTime time.Duration
	var downloadCount, uploadCount int

	for i := 0; i < st.config.Concurrent; i++ {
		if dr := <-downloadResults; dr != nil {
			totalDownloadBytes += dr.bytes
			totalDownloadTime += dr.duration
			downloadCount++
		}
	}
	close(downloadResults)

	for i := 0; i < st.config.Concurrent; i++ {
		if ur := <-uploadResults; ur != nil {
			totalUploadBytes += ur.bytes
			totalUploadTime += ur.duration
			uploadCount++
		}
	}
	close(uploadResults)

	if downloadCount > 0 {
		result.DownloadSize = float64(totalDownloadBytes)
		result.DownloadTime = totalDownloadTime / time.Duration(downloadCount)
		result.DownloadSpeed = float64(totalDownloadBytes) / result.DownloadTime.Seconds()
	}
	if uploadCount > 0 {
		result.UploadSize = float64(totalUploadBytes)
		result.UploadTime = totalUploadTime / time.Duration(uploadCount)
		result.UploadSpeed = float64(totalUploadBytes) / result.UploadTime.Seconds()
	}

	return result
}

type latencyResult struct {
	avgLatency time.Duration
	jitter     time.Duration
	packetLoss float64
}

func (st *SpeedTester) testLatency(proxy constant.Proxy) *latencyResult {
	client := st.createClient(proxy)
	const testCount = 2 //diy 延迟测试次数
	latencies := make([]time.Duration, 0, testCount) //diy
	failedPings := 0

	for i := 0; i < testCount; i++ { //diy
		time.Sleep(100 * time.Millisecond)

		start := time.Now()
		resp, err := client.Get(fmt.Sprintf("%s/__down?bytes=0", st.config.ServerURL))
		if err != nil {
			failedPings++
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			latencies = append(latencies, time.Since(start))
		} else {
			failedPings++
		}
	}

	return calculateLatencyStats(latencies, failedPings, testCount) //diy
}

type downloadResult struct {
	bytes    int64
	duration time.Duration
}

func (st *SpeedTester) testDownload(proxy constant.Proxy, size int) *downloadResult {
	client := st.createClient(proxy)
	start := time.Now()

	resp, err := client.Get(fmt.Sprintf("%s/__down?bytes=%d", st.config.ServerURL, size))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	downloadBytes, _ := io.Copy(io.Discard, resp.Body)

	return &downloadResult{
		bytes:    downloadBytes,
		duration: time.Since(start),
	}
}

func (st *SpeedTester) testUpload(proxy constant.Proxy, size int) *downloadResult {
	client := st.createClient(proxy)
	reader := NewZeroReader(size)

	start := time.Now()
	resp, err := client.Post(
		fmt.Sprintf("%s/__up", st.config.ServerURL),
		"application/octet-stream",
		reader,
	)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	return &downloadResult{
		bytes:    reader.WrittenBytes(),
		duration: time.Since(start),
	}
}

func (st *SpeedTester) createClient(proxy constant.Proxy) *http.Client {
	return &http.Client{
		Timeout: st.config.Timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var u16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err == nil {
					u16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &constant.Metadata{
					Host:    host,
					DstPort: u16Port,
				})
			},
		},
	}
}

func calculateLatencyStats(latencies []time.Duration, failedPings int, testCount int) *latencyResult { //diy
	result := &latencyResult{
		packetLoss: float64(failedPings) / float64(testCount) * 100, //diy
	}

	if len(latencies) == 0 {
		return result
	}

	// 计算平均延迟
	var total time.Duration
	for _, l := range latencies {
		total += l
	}
	result.avgLatency = total / time.Duration(len(latencies))

	// 计算抖动
	var variance float64
	for _, l := range latencies {
		diff := float64(l - result.avgLatency)
		variance += diff * diff
	}
	variance /= float64(len(latencies))
	result.jitter = time.Duration(math.Sqrt(variance))

	return result
}
