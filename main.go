package main

import (
	"bytes"
	"flag"
	"fmt"
	"github.com/songgao/water"
	"gopkg.in/yaml.v3"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// 定数定義
const (
	etherIPProto     = 97               // EtherIPのプロトコル番号（RFC3378準拠）
	bufferSize       = 131070           // バッファサイズ
	retryOnFailDelay = 30 * time.Second // DNS解決失敗時の再試行間隔
	sendWorkerCount  = 4                // 送信goroutine数
	recvWorkerCount  = 4                // 受信goroutine数
	sendChanSize     = 100              // 送信チャネルバッファサイズ
	recvChanSize     = 100              // 受信チャネルバッファサイズ
)

// ログ出力用のカラーコード定義
var colors = map[string]string{
	"[INFO]":   "\033[0m",  // デフォルト
	"[WARN]":   "\033[33m", // 黄色
	"[ERROR]":  "\033[31m", // 赤
	"[UPDATE]": "\033[32m", // 緑
	"[RESET]":  "\033[35m", // 紫
}

// logf はカラー付きのログ出力を行う
func logf(tag, format string, a ...interface{}) {
	color, ok := colors[tag]
	if !ok {
		color = "\033[0m"
	}
	fmt.Printf("%s%s %s\033[0m\n", color, tag, fmt.Sprintf(format, a...))
}

// Configは設定ファイルから読み取る情報を保持する
type Config struct {
	Version         int    `yaml:"version"`          // IPv4 or IPv6 (4 or 6)
	TapName         string `yaml:"tap_name"`         // TAPインターフェース名
	BrName          string `yaml:"br_name"`          // ブリッジ名（"off"で無効）
	MTU             int    `yaml:"mtu"`              // MTUサイズ
	SrcIface        string `yaml:"src_iface"`        // 送信元インターフェース名
	DstHost         string `yaml:"dst_host"`         // 送信先ホスト名またはIP
	ResolveInterval string `yaml:"resolve_interval"` // DNS再解決間隔
}

// Packetはパケットデータを格納するための構造体
type Packet struct {
	Data   []byte
	Offset int
	Length int
	Pool   *sync.Pool
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	configPath := "config.yaml"
	flag.Parse()

	cfg, err := loadConfig(configPath)
	if err != nil {
		logf("[ERROR]", "Failed to load config: %v", err)
		os.Exit(1)
	}

	interval, err := time.ParseDuration(cfg.ResolveInterval)
	if err != nil {
		logf("[ERROR]", "Invalid resolve_interval: %v", err)
		os.Exit(1)
	}

	// TAPインターフェース作成
	ifce, err := water.New(water.Config{DeviceType: water.TAP})
	if err != nil {
		logf("[ERROR]", "TAP create: %v", err)
		os.Exit(1)
	}
	defer ifce.Close()

	actualName := ifce.Name()

	// 目的のTAPインターフェース名が既に存在している場合の対処
	if actualName != cfg.TapName {
		if ifaceExists(cfg.TapName) {
			logf("[ERROR]", "TAP interface name '%s' already exists. Choose a different name or remove the existing interface.", cfg.TapName)
			os.Exit(1)
		}

		// インターフェースの名前変更を実行
		if err := renameInterface(actualName, cfg.TapName); err != nil {
			logf("[ERROR]", "Rename TAP: %v", err)
			os.Exit(1)
		}
	}

	if err := linkUp(cfg.TapName); err != nil {
		logf("[ERROR]", "TAP UP: %v", err)
		os.Exit(1)
	}

	if err := setTAPMTU(cfg.TapName, cfg.MTU); err != nil {
		logf("[ERROR]", "MTU: %v", err)
		os.Exit(1)
	}

	// ブリッジへの自動参加処理
	if cfg.BrName != "off" {
		if err := addToBridge(cfg.TapName, cfg.BrName); err != nil {
			logf("[ERROR]", "Failed to add %s to bridge %s: %v", cfg.TapName, cfg.BrName, err)
			os.Exit(1)
		}
		logf("[INFO]", "TAP interface %s joined bridge %s", cfg.TapName, cfg.BrName)
	}

	srcIP, err := getInterfaceIP(cfg.SrcIface, cfg.Version)
	if err != nil {
		logf("[ERROR]", "Source IP: %v", err)
		os.Exit(1)
	}

	dstIPVal := atomic.Value{}
	firstDst, err := resolveDst(cfg.DstHost, cfg.Version)
	if err != nil {
		logf("[ERROR]", "Resolve %s: %v", cfg.DstHost, err)
		os.Exit(1)
	}
	dstIPVal.Store(firstDst)

	// 宛先の定期的なDNS再解決処理開始goroutine
	go startDynamicResolver(cfg.DstHost, cfg.Version, interval, &dstIPVal)

	proto := fmt.Sprintf("ip%d:%d", cfg.Version, etherIPProto)
	rawConn, err := net.ListenIP(proto, &net.IPAddr{IP: srcIP})
	if err != nil {
		logf("[ERROR]", "RAW socket: %v", err)
		os.Exit(1)
	}
	defer rawConn.Close()

	logf("[INFO]", "EtherIP Tunnel started")
	logf("[INFO]", "TAP: %s | MTU: %d", cfg.TapName, cfg.MTU)
	logf("[INFO]", "SRC: %s (%s) → DST: %s (%s)", srcIP, cfg.SrcIface, firstDst, cfg.DstHost)

	sendPool := &sync.Pool{New: func() interface{} { return make([]byte, bufferSize) }}
	recvPool := &sync.Pool{New: func() interface{} { return make([]byte, bufferSize) }}

	// 送信/受信用チャネル
	sendChan := make(chan Packet, sendChanSize)
	recvChan := make(chan Packet, recvChanSize)

	// TAPから読み取り、送信チャネルへ送る
	go func() {
		for {
			buf := sendPool.Get().([]byte)
			n, err := ifce.Read(buf)
			if err != nil {
				logf("[ERROR]", "TAP read: %v", err)
				sendPool.Put(buf)
				continue
			}
			sendChan <- Packet{buf, 0, n, sendPool}
		}
	}()

	// RAWソケットから受信チャネルへ送る
	go func() {
		for {
			buf := recvPool.Get().([]byte)
			n, _, err := rawConn.ReadFrom(buf)
			if err != nil || n < 2 || buf[0]>>4 != 3 || buf[0]&0x0F != 0 || buf[1] != 0 {
				recvPool.Put(buf)
				continue
			}
			recvChan <- Packet{buf, 2, n - 2, recvPool}
		}
	}()

	// 送信処理ワーカーgoroutine
	var wg sync.WaitGroup
	for i := 0; i < sendWorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pkt := range sendChan {
				packet := buildEtherIPPacket(pkt.Data[:pkt.Length])
				currentDst := dstIPVal.Load().(net.IP)
				rawConn.WriteTo(packet, &net.IPAddr{IP: currentDst})
				pkt.Pool.Put(pkt.Data)
			}
		}()
	}

	// 受信処理ワーカーgoroutine
	for i := 0; i < recvWorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pkt := range recvChan {
				ifce.Write(pkt.Data[pkt.Offset : pkt.Offset+pkt.Length])
				pkt.Pool.Put(pkt.Data)
			}
		}()
	}

	// メインスレッドは終了せず、ワーカー終了待ち（永続）
	wg.Wait()
}

// loadConfig は YAML設定ファイルを読み込み、Config構造体に格納する
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		logf("[ERROR]", "Failed to read config file: %v", err)
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logf("[ERROR]", "Failed to parse config file: %v", err)
		return nil, err
	}

	// デフォルト値を設定（設定漏れ防止）
	if cfg.MTU == 0 {
		cfg.MTU = 1500
		logf("[INFO]", "MTU not specified, defaulting to 1500")
	}
	if cfg.ResolveInterval == "" {
		cfg.ResolveInterval = "10s"
		logf("[INFO]", "ResolveInterval not specified, defaulting to 10s")
	}
	if cfg.TapName == "" {
		cfg.TapName = "tap0"
		logf("[INFO]", "TapName not specified, defaulting to tap0")
	}
	if cfg.BrName == "" {
		cfg.BrName = "off"
		logf("[INFO]", "BrName not specified, defaulting to off")
	}

	return &cfg, nil
}

// buildEtherIPPacket は EtherIPヘッダを付与したパケットを生成する関数
func buildEtherIPPacket(frame []byte) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x30, 0x00}) // EtherIP ヘッダ (Version=3, Reserved=0)
	buf.Write(frame)
	return buf.Bytes()
}

// renameInterface はインターフェースの名前を変更する関数
func renameInterface(oldName, newName string) error {
	if err := exec.Command("ip", "link", "set", oldName, "name", newName).Run(); err != nil {
		logf("[ERROR]", "Failed to rename interface: %v", err)
		return err
	}
	logf("[INFO]", "Interface renamed from %s to %s", oldName, newName)
	return nil
}

// ifaceExists は指定された名前のインターフェースが存在するか確認する関数
func ifaceExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// linkUp はインターフェースを有効(UP)にする関数
func linkUp(ifname string) error {
	if err := exec.Command("ip", "link", "set", "dev", ifname, "up").Run(); err != nil {
		logf("[ERROR]", "Failed to set interface %s UP: %v", ifname, err)
		return err
	}
	logf("[INFO]", "Interface %s set UP", ifname)
	return nil
}

// setTAPMTU はインターフェースのMTUを設定する関数
func setTAPMTU(name string, mtu int) error {
	if err := exec.Command("ip", "link", "set", "dev", name, "mtu", fmt.Sprintf("%d", mtu)).Run(); err != nil {
		logf("[ERROR]", "Failed to set MTU on interface %s: %v", name, err)
		return err
	}
	logf("[INFO]", "MTU of interface %s set to %d", name, mtu)
	return nil
}

// addToBridge はTAPインターフェースを指定したブリッジに追加する関数
func addToBridge(ifname, brname string) error {
	if err := exec.Command("ip", "link", "set", "dev", ifname, "master", brname).Run(); err != nil {
		logf("[ERROR]", "Failed to add interface %s to bridge %s: %v", ifname, brname, err)
		return err
	}
	logf("[INFO]", "Interface %s added to bridge %s", ifname, brname)
	return nil
}

// getInterfaceIP は指定されたインターフェースからIPv4またはIPv6のIPアドレスを取得する関数
func getInterfaceIP(ifname string, version int) (net.IP, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		logf("[ERROR]", "Interface %s not found: %v", ifname, err)
		return nil, err
	}

	addrs, _ := iface.Addrs()
	for _, addr := range addrs {
		ip, _, _ := net.ParseCIDR(addr.String())
		if version == 4 && ip.To4() != nil {
			logf("[INFO]", "IPv4 address found on %s: %s", ifname, ip)
			return ip, nil
		}
		if version == 6 && ip.To16() != nil && ip.To4() == nil {
			logf("[INFO]", "IPv6 address found on %s: %s", ifname, ip)
			return ip, nil
		}
	}

	err = fmt.Errorf("no suitable IP found for IPv%d on %s", version, ifname)
	logf("[ERROR]", "%v", err)
	return nil, err
}

// resolveDst は宛先のFQDNをIPアドレスにDNS解決する関数
func resolveDst(host string, version int) (net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		logf("[ERROR]", "DNS lookup failed for host %s: %v", host, err)
		return nil, err
	}

	for _, ip := range ips {
		if version == 4 && ip.To4() != nil {
			// logf("[INFO]", "Resolved IPv4 %s → %s", host, ip)
			return ip, nil
		}
		if version == 6 && ip.To16() != nil && ip.To4() == nil {
			// logf("[INFO]", "Resolved IPv6 %s → %s", host, ip)
			return ip, nil
		}
	}

	err = fmt.Errorf("no suitable IP found for host %s (IPv%d)", host, version)
	logf("[ERROR]", "%v", err)
	return nil, err
}

// startDynamicResolver は宛先IPを定期的にDNS再解決する関数
func startDynamicResolver(host string, version int, interval time.Duration, dstVal *atomic.Value) {
	for {
		time.Sleep(interval)
		for {
			newIP, err := resolveDst(host, version)
			if err != nil {
				logf("[WARN]", "DNS resolve failed for %s: %v, retry in %v", host, err, retryOnFailDelay)
				time.Sleep(retryOnFailDelay)
				continue
			}

			old := dstVal.Load().(net.IP)
			if !old.Equal(newIP) {
				logf("[UPDATE]", "DNS updated: %s → %s", old, newIP)
				dstVal.Store(newIP)
			}
			break
		}
	}
}
