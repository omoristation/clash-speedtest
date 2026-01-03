package speedtester

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url" //diy
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/provider"
	"github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/common/utils" //diy
	"gopkg.in/yaml.v3"
)

type Config struct {
	ConfigPaths      string
	FilterRegex      string
	BlockRegex       string
	ServerURL        string
	DownloadSize     int
	UploadSize       int
	Timeout          time.Duration
	Concurrent       int
	MaxLatency       time.Duration
	MinDownloadSpeed float64
	MinUploadSpeed   float64
	FastMode         bool
	LatencyURL       string //diy
	TestCount        int //diy
	MaxRetries       int //diy
	Debug            bool //diy
}

type SpeedTester struct {
	config           *Config
	blockedNodes     []string
	blockedNodeCount int
}

func New(config *Config) *SpeedTester {
	if config.Concurrent <= 0 {
		config.Concurrent = 1
	}
	if config.DownloadSize < 0 {
		config.DownloadSize = 100 * 1024 * 1024
	}
	if config.UploadSize < 0 {
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

func (st *SpeedTester) LoadProxies(stashCompatible bool) (map[string]*CProxy, error) {
	allProxies := make(map[string]*CProxy)
	st.blockedNodes = make([]string, 0)
	st.blockedNodeCount = 0
	//1.如果配置同时存在http和yaml，并且yaml的修改时间小于1小时,就使用yaml,如果大于1小时,就下载HTTP的保存为YAML的本地路径，再使用这个本地YAML配置 2.如果只存在HTTP或者YAML配置，就单独使用这个配置
	//diy 分离HTTP和YAML配置路径
	var httpConfig, yamlConfig string
	for _, configPath := range strings.Split(st.config.ConfigPaths, ",") {
		if strings.HasPrefix(configPath, "http") {
			httpConfig = configPath
		} else {
			yamlConfig = configPath
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
		var resp *http.Response
		resp, err = http.Get(configPath)
		if err != nil {
			log.Warnln("failed to fetch config: %s", err)
			//continue //diy
		}
		body, err = io.ReadAll(resp.Body)
	} else {
		body, err = os.ReadFile(configPath)
	}
	if err != nil {
		log.Warnln("failed to read config: %s", err)
		//continue //diy
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

		resp, err := http.Get(config["url"].(string))
		if err != nil {
			log.Warnln("failed to fetch config: %s", err)
			continue
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		pdRawCfg := &RawConfig{
			Proxies: []map[string]any{},
		}
		if err := yaml.Unmarshal(body, pdRawCfg); err != nil {
			return nil, err
		}
		pdProxies := make(map[string]map[string]any)
		for _, pdProxy := range pdRawCfg.Proxies {
			pdProxies[pdProxy["name"].(string)] = pdProxy
		}
		for _, proxy := range pd.Proxies() {
			proxies[fmt.Sprintf("[%s] %s", name, proxy.Name())] = &CProxy{
				Proxy:  proxy,
				Config: pdProxies[proxy.Name()],
			}
		}
	}

	// 过滤代理
	for k, p := range proxies {
		switch p.Type() {
			case constant.Shadowsocks, constant.ShadowsocksR, constant.Snell, constant.Socks5, constant.Http,
				constant.Vmess, constant.Vless, constant.Trojan, constant.Hysteria, constant.Hysteria2,
				constant.WireGuard, constant.Tuic, constant.Ssh, constant.Mieru, constant.AnyTLS:
			default:
			continue
		}
		if server, ok := p.Config["server"]; ok {
			p.Config["server"] = convertMappedIPv6ToIPv4(server.(string))
		}
		if stashCompatible && !isStashCompatible(p) {
			continue
		}
		if _, ok := allProxies[k]; !ok {
			allProxies[k] = p
		}
	}

	// 应用正则过滤
	filterRegexp := regexp.MustCompile(st.config.FilterRegex)
	var blockKeywords []string
	if st.config.BlockRegex != "" {
		for _, keyword := range strings.Split(st.config.BlockRegex, "|") {
			keyword = strings.TrimSpace(keyword)
			if keyword != "" {
				blockKeywords = append(blockKeywords, strings.ToLower(keyword))
			}
		}
	}

	filteredProxies := make(map[string]*CProxy)
	for name := range allProxies {
		shouldBlock := false
		if len(blockKeywords) > 0 {
			lowerName := strings.ToLower(name)
			for _, keyword := range blockKeywords {
				if strings.Contains(lowerName, keyword) {
					shouldBlock = true
					break
				}
			}
		}

		if shouldBlock {
			continue
		}
		if filterRegexp.MatchString(name) {
			filteredProxies[name] = allProxies[name]
		}
	}
	return filteredProxies, nil
}

func isStashCompatible(proxy *CProxy) bool {
	switch proxy.Type() {
	case constant.Shadowsocks:
		cipher, ok := proxy.Config["cipher"]
		if ok {
			switch cipher {
			case "aes-128-gcm", "aes-192-gcm", "aes-256-gcm",
				"aes-128-cfb", "aes-192-cfb", "aes-256-cfb",
				"aes-128-ctr", "aes-192-ctr", "aes-256-ctr",
				"rc4-md5", "chacha20", "chacha20-ietf", "xchacha20",
				"chacha20-ietf-poly1305", "xchacha20-ietf-poly1305",
				"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm":
			default:
				return false
			}
		}
	case constant.ShadowsocksR:
		if obfs, ok := proxy.Config["obfs"]; ok {
			switch obfs {
			case "plain", "http_simple", "http_post", "random_head",
				"tls1.2_ticket_auth", "tls1.2_ticket_fastauth":
			default:
				return false
			}
		}
		if protocol, ok := proxy.Config["protocol"]; ok {
			switch protocol {
			case "origin", "auth_sha1_v4", "auth_aes128_md5",
				"auth_aes128_sha1", "auth_chain_a", "auth_chain_b":
			default:
				return false
			}
		}
	case constant.Snell:
		if obfsOpts, ok := proxy.Config["obfs-opts"]; ok {
			if obfsOptsMap, ok := obfsOpts.(map[string]any); ok {
				if mode, ok := obfsOptsMap["mode"]; ok {
					switch mode {
					case "http", "tls":
					default:
						return false
					}
				}
			}
		}
	case constant.Socks5, constant.Http:
	case constant.Vmess:
		if cipher, ok := proxy.Config["cipher"]; ok {
			switch cipher {
			case "auto", "aes-128-gcm", "chacha20-poly1305", "none":
			default:
				return false
			}
		}
		if network, ok := proxy.Config["network"]; ok {
			switch network {
			case "ws", "h2", "http", "grpc":
			default:
				return false
			}
		}
	case constant.Vless:
		if flow, ok := proxy.Config["flow"]; ok {
			switch flow {
			case "xtls-rprx-origin", "xtls-rprx-direct", "xtls-rprx-splice", "xtls-rprx-vision":
			default:
				return false
			}
		}
	case constant.Trojan:
		if network, ok := proxy.Config["network"]; ok {
			switch network {
			case "ws", "grpc":
			default:
				return false
			}
		}
	case constant.Hysteria, constant.Hysteria2:
	case constant.WireGuard:
	case constant.Tuic:
	case constant.Ssh:
	default:
		return false
	}
	return true
}

func (st *SpeedTester) TestProxies(proxies map[string]*CProxy, tester func(result *Result)) {
	for name, proxy := range proxies {
		tester(st.testProxy(name, proxy))
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
	latencyResult := st.testLatency(proxy, st.config.MaxLatency)
	result.Latency = latencyResult.avgLatency
	// 如果是快速模式，只测试延迟，直接返回结果 diy
	if st.config.FastMode {
		return result
	} else {
		result.Jitter = latencyResult.jitter
		result.PacketLoss = latencyResult.packetLoss
	}

	//diy 如果延迟测试完全失败，直接返回
	if result.PacketLoss == 100 || result.Latency > st.config.MaxLatency {
		return result
	}

	// 2. 并发进行下载和上传测试

	var wg sync.WaitGroup

	var totalDownloadBytes, totalUploadBytes int64
	var totalDownloadTime, totalUploadTime time.Duration
	var downloadCount, uploadCount int

	downloadChunkSize := st.config.DownloadSize / st.config.Concurrent
	if downloadChunkSize > 0 {
		downloadResults := make(chan *downloadResult, st.config.Concurrent)

		for i := 0; i < st.config.Concurrent; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				downloadResults <- st.testDownload(proxy, downloadChunkSize, st.config.Timeout)
			}()
		}
		wg.Wait()

		for range st.config.Concurrent {
			if dr := <-downloadResults; dr != nil {
				totalDownloadBytes += dr.bytes
				totalDownloadTime += dr.duration
				downloadCount++
			}
		}
		close(downloadResults)

		if downloadCount > 0 {
			result.DownloadSize = float64(totalDownloadBytes)
			result.DownloadTime = totalDownloadTime / time.Duration(downloadCount)
			result.DownloadSpeed = float64(totalDownloadBytes) / result.DownloadTime.Seconds()
		}

		if result.DownloadSpeed < st.config.MinDownloadSpeed {
			return result
		}
	}

	uploadChunkSize := st.config.UploadSize / st.config.Concurrent
	if uploadChunkSize > 0 {
		uploadResults := make(chan *downloadResult, st.config.Concurrent)

		for i := 0; i < st.config.Concurrent; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				uploadResults <- st.testUpload(proxy, uploadChunkSize, st.config.Timeout)
			}()
		}
		wg.Wait()

		for i := 0; i < st.config.Concurrent; i++ {
			if ur := <-uploadResults; ur != nil {
				totalUploadBytes += ur.bytes
				totalUploadTime += ur.duration
				uploadCount++
			}
		}
		close(uploadResults)

		if uploadCount > 0 {
			result.UploadSize = float64(totalUploadBytes)
			result.UploadTime = totalUploadTime / time.Duration(uploadCount)
			result.UploadSpeed = float64(totalUploadBytes) / result.UploadTime.Seconds()
		}

		if result.UploadSpeed < st.config.MinUploadSpeed {
			return result
		}
	}

	return result
}

type latencyResult struct {
	avgLatency time.Duration
	jitter     time.Duration
	packetLoss float64
}

func (st *SpeedTester) testLatency(proxy constant.Proxy, minLatency time.Duration) *latencyResult {
	testCount := st.config.TestCount // 测试次数，默认1次
	var latencies []time.Duration // 收集成功延迟
	var failedPings int // 失败计数
	failedPingsThisURL := 0
	// 支持多个备用 LatencyURL，用逗号分隔
	LatencyURLs := strings.Split(st.config.LatencyURL, ",")
	if len(LatencyURLs) == 0 {
		LatencyURLs = []string{"http://1.1.1.1"} // 防止空值
	}

	// 依次尝试每个 LatencyURL，直到成功获取到至少一次延迟或全部失败
	for _, targetURL := range LatencyURLs {
		targetURL = strings.TrimSpace(targetURL)
		if targetURL == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), st.config.Timeout) // 总上下文
		defer cancel()

		for i := 0; i < testCount; i++ {
			t, satisfied, err := st.IndependentURLTest(proxy, ctx, targetURL, nil) // 调用独立函数，nil忽略状态码
			if err != nil || !satisfied {
				failedPingsThisURL++ // 失败计数（含重试失败）
				if st.config.Debug {
					fmt.Printf("%s 第%d轮测试失败\n", targetURL, i+1) // 可选日志
				}
			} else {
				latencies = append(latencies, time.Duration(t)*time.Millisecond) // 成功延迟
				if st.config.Debug {
					fmt.Printf("%s 第%d轮 延迟:%dms\n", targetURL, i+1, t) // 可选日志
				}
			}
		}
		failedPings += failedPingsThisURL

		// 如果本次 URL 有至少一次成功，则认为延迟测试成功，后面的备用 URL 不需要再测
		if len(latencies) > 0 {
			if st.config.Debug {
				fmt.Printf("使用 %s 获取到有效延迟，停止尝试后续备用地址\n", targetURL)
			}
			break
		}

		if st.config.Debug {
			fmt.Printf("%s 全部 %d 次测试失败，尝试下一个备用地址\n", targetURL, testCount)
		}
	}

	// 计算最终统计结果
	//totalTests := len(LatencyURLs) * testCount // 实际尝试的总次数（可能因提前成功而少）
	// 但为了丢包率准确，我们仍以配置的 testCount 为基准（只算实际尝试的轮次更合理，这里按实际计算）
	actualTotalTests := len(LatencyURLs) * testCount
	if len(latencies) > 0 {
		actualTotalTests = testCount // 有成功时，只算第一次成功的那个 URL 的 testCount
	}

	return calculateLatencyStats(latencies, failedPings, actualTotalTests) //diy
}

type downloadResult struct {
	bytes    int64
	duration time.Duration
}

func (st *SpeedTester) testDownload(proxy constant.Proxy, size int, timeout time.Duration) *downloadResult {
	client := st.createClient(proxy, timeout)
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

func (st *SpeedTester) testUpload(proxy constant.Proxy, size int, timeout time.Duration) *downloadResult {
	client := st.createClient(proxy, timeout)
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

func (st *SpeedTester) createClient(proxy constant.Proxy, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
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
			MaxIdleConns:          100,   //diy 只需一个连接
			IdleConnTimeout:       30 * time.Second, //diy
			TLSHandshakeTimeout:   10 * time.Second, //diy
			ExpectContinueTimeout: 1 * time.Second, //diy
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error { //diy
			return http.ErrUseLastResponse
		},
	}
}

func calculateLatencyStats(latencies []time.Duration, failedPings int, testCount int) *latencyResult { //diy 函数（计算统计：平均、丢包率）
	result := &latencyResult{
		packetLoss: float64(failedPings) / float64(testCount) * 100, //diy 丢包率计算
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

func convertMappedIPv6ToIPv4(server string) string {
	ip := net.ParseIP(server)
	if ip == nil {
		return server
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}
	return server
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
// urlToMetadata 独立移植：将URL解析为Metadata（原Clash逻辑简化）
func urlToMetadata(targetURL string) (constant.Metadata, error) {
	parsedURL, err := url.Parse(targetURL) // 需import "net/url" 使用包url.Parse，避免冲突 // 参数重命名为targetURL
	if err != nil {
		return constant.Metadata{}, err
	}
	host := parsedURL.Hostname()
	portStr := parsedURL.Port()
	var dstPort uint16
	if portStr == "" {
		dstPort = 80 // 默认HTTP端口
		if parsedURL.Scheme == "https" {
			dstPort = 443
		}
	} else if portNum, err := strconv.Atoi(portStr); err == nil {
		dstPort = uint16(portNum)
	}
	return constant.Metadata{
		Host:    host,
		DstPort: dstPort,
	}, nil
}

// IndependentURLTest 独立移植的URLTest函数（含重试机制）
// 参数：proxy - 您的constant.Proxy；ctx - 上下文；targetURL - 测试URL；expectedStatus - 期望状态码（nil忽略）
// 返回：t - ms级延迟；satisfied - 是否满足条件；err - 错误（全重试失败时err!=nil）
func (st *SpeedTester) IndependentURLTest(proxy constant.Proxy, ctx context.Context, targetURL string, expectedStatus *utils.IntRanges[uint16]) (t uint16, satisfied bool, err error) {
	// defer逻辑简化：无Clash状态更新，直接返回satisfied
	defer func() {
		if err != nil || !satisfied {
			t = 0 // 失败时延迟设0
		}
	}()

	maxRetries := st.config.MaxRetries // 最大重试次数 默认5次
	const retryInterval = 500 * time.Millisecond // 重试间隔

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// 创建子上下文，确保总超时
		testCtx, testCancel := context.WithTimeout(ctx, 30*time.Second) // 单次测试超时
		singleT, singleSatisfied, singleErr := performSingleTest(proxy, testCtx, targetURL, expectedStatus)
		testCancel()

		if singleErr == nil && singleSatisfied {
			// 成功：返回首次成功结果
			return singleT, true, nil
		}

		// 失败：日志（可选，仅首次）
		if attempt == 0 {
			if st.config.Debug {
				fmt.Printf("首次测试失败... (URL:%s, 错误:%s)", targetURL, singleErr)
			}
		}

		// 非最后一次：等待重试
		if attempt < maxRetries {
			time.Sleep(retryInterval)
		}
	}

	// 全重试失败
	if st.config.Debug {
		fmt.Printf("所有重试失败 (URL:%s)", targetURL)
	}
	return 0, false, fmt.Errorf("测试失败：所有重试均未成功")
}
// performSingleTest 内部辅助函数：执行单次测试逻辑（提取，便于重试）
func performSingleTest(proxy constant.Proxy, ctx context.Context, targetURL string, expectedStatus *utils.IntRanges[uint16]) (uint16, bool, error) {
	addr, err := urlToMetadata(targetURL)
	if err != nil {
		return 0, false, err
	}

	start := time.Now()
	instance, err := proxy.DialContext(ctx, &addr) // 预拨连接
	if err != nil {
		return 0, false, err
	}
	defer func() {
		_ = instance.Close() // 关闭预拨连接
	}()

	req, err := http.NewRequest(http.MethodHead, targetURL, nil) // HEAD请求，使用targetURL
	if err != nil {
		return 0, false, err
	}
	req = req.WithContext(ctx) // 绑定上下文


	if err != nil {
		return 0, false, err
	}

	// 自定义Transport：固定返回预拨连接（原Clash逻辑）
	transport := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return instance, nil // 复用预拨连接
		},
		// 从http.DefaultTransport继承
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		//TLSClientConfig:     tlsConfig,
	}

	client := &http.Client{
		Timeout:       30 * time.Second, // 超时
		Transport:     transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 禁用重定向
		},
	}
	defer client.CloseIdleConnections() // 清理空闲连接

	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	_ = resp.Body.Close() // 关闭Body（HEAD无body）

	// unifiedDelay逻辑：第二个请求重置start
	secondStart := time.Now()
	var ignoredErr error
	var secondResp *http.Response
	secondResp, ignoredErr = client.Do(req) // 第二个HEAD
	if ignoredErr == nil {
		resp = secondResp
		_ = resp.Body.Close()
		start = secondStart // 重置start，只计第二个RTT
	}

	// 计算satisfied和t
	satisfied := resp != nil && (expectedStatus == nil || expectedStatus.Check(uint16(resp.StatusCode)))
	t := uint16(time.Since(start) / time.Millisecond) // ms级延迟

	return t, satisfied, nil
}