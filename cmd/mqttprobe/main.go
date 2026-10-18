// mqttprobe —— 用 Eclipse Paho 直连 MQTT broker 探活 + 周期性发布/订阅测试消息。
//
// 双协议支持：
//   -protocol-version v5       → github.com/eclipse/paho.golang (autopaho，自动重连)
//   -protocol-version v3.1.1   → github.com/eclipse/paho.mqtt.golang (auto-reconnect)
//
// 用法：
//   go run ./cmd/mqttprobe
//   go run ./cmd/mqttprobe -broker tcp://127.0.0.1:1883 -topic plcbridge/probe
//   go run ./cmd/mqttprobe -broker tcp://127.0.0.1:1883 -protocol-version v3.1.1 -sub
//   go run ./cmd/mqttprobe -broker tls://broker.hivemq.com:8883 -client-id probe-01
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pahov5 "github.com/eclipse/paho.golang/autopaho"
	pahov5paho "github.com/eclipse/paho.golang/paho"
	pahov311 "github.com/eclipse/paho.mqtt.golang"
)

func main() {
	broker := flag.String("broker", "tcp://127.0.0.1:1883", "MQTT broker URL (tcp/mqtt/tls/ssl/ws/wss)")
	clientID := flag.String("client-id", "mqttprobe", "MQTT ClientID")
	username := flag.String("username", "", "Username")
	password := flag.String("password", "", "Password")
	topic := flag.String("topic", "plcbridge/probe", "Publish/Subscribe topic")
	keepalive := flag.Int("keepalive", 30, "Keepalive (sec)")
	qos := flag.Int("qos", 1, "QoS (0/1/2)")
	retained := flag.Bool("retained", false, "Retain flag")
	subscribe := flag.Bool("sub", false, "Subscribe to -topic instead of publish")
	timeout := flag.Duration("timeout", 10*time.Second, "Initial connect timeout")
	protocolVersion := flag.String("protocol-version", "v5", "MQTT protocol version: v5 | v3.1.1")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n[mqttprobe] interrupted")
		cancel()
	}()

	brokerURL, err := normalizeBrokerURL(*broker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mqttprobe] broker URL 解析失败: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("[mqttprobe] connecting to %s protocol=%s ...\n", brokerURL, *protocolVersion)

	switch *protocolVersion {
	case "v5":
		runV5(ctx, brokerURL, *clientID, *username, *password, *topic,
			*keepalive, byte(*qos), *retained, *subscribe, *timeout)
	case "v3.1.1", "v311":
		runV311(ctx, brokerURL, *clientID, *username, *password, *topic,
			*keepalive, byte(*qos), *retained, *subscribe, *timeout)
	default:
		fmt.Fprintf(os.Stderr, "[mqttprobe] 不支持的 protocol-version: %s（仅 v5 / v3.1.1）\n", *protocolVersion)
		os.Exit(2)
	}
}

// === v5（autopaho）===

func runV5(ctx context.Context, brokerURL *url.URL, clientID, username, password, topic string,
	keepalive int, qos byte, retained, subscribe bool, timeout time.Duration) {

	cliCfg := pahov5.ClientConfig{
		ServerUrls:           []*url.URL{brokerURL},
		KeepAlive:            uint16(keepalive),
		ConnectUsername:      username,
		ConnectPassword:      []byte(password),
		ConnectTimeout:       timeout,
		ReconnectBackoff:     func(int) time.Duration { return 2 * time.Second },
		OnConnectionUp: func(cm *pahov5.ConnectionManager, _ *pahov5paho.Connack) {
			fmt.Println("[mqttprobe] connected ✓ (v5)")
			if subscribe {
				if _, err := cm.Subscribe(ctx, &pahov5paho.Subscribe{
					Subscriptions: []pahov5paho.SubscribeOptions{
						{Topic: topic, QoS: qos},
					},
				}); err != nil {
					fmt.Fprintf(os.Stderr, "[mqttprobe] subscribe failed: %v\n", err)
				} else {
					fmt.Printf("[mqttprobe] subscribed to %s\n", topic)
				}
			}
		},
		OnConnectError: func(err error) { fmt.Fprintf(os.Stderr, "[mqttprobe] connect error: %v\n", err) },
		ClientConfig: pahov5paho.ClientConfig{
			ClientID: clientID,
			OnPublishReceived: []func(pahov5paho.PublishReceived) (bool, error){
				func(pr pahov5paho.PublishReceived) (bool, error) {
					fmt.Printf("[mqttprobe] received topic=%s body=%s retain=%t\n",
						pr.Packet.Topic, string(pr.Packet.Payload), pr.Packet.Retain)
					return true, nil
				},
			},
		},
	}

	cm, err := pahov5.NewConnection(ctx, cliCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mqttprobe] NewConnection 失败: %v\n", err)
		os.Exit(2)
	}
	connectCtx, connectCancel := context.WithTimeout(ctx, timeout)
	defer connectCancel()
	if err := cm.AwaitConnection(connectCtx); err != nil {
		fmt.Fprintf(os.Stderr, "[mqttprobe] AwaitConnection 失败: %v\n", err)
		os.Exit(2)
	}
	defer func() { <-cm.Done() }()

	if subscribe {
		fmt.Println("[mqttprobe] waiting for messages (Ctrl+C to stop) ...")
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	count := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Println("[mqttprobe] exit")
			return
		case <-ticker.C:
			count++
			payload := fmt.Sprintf(`{"ts":"%s","seq":%d,"hello":"mqttprobe","proto":"v5"}`,
				time.Now().UTC().Format(time.RFC3339Nano), count)
			if _, err := cm.Publish(ctx, &pahov5paho.Publish{
				QoS: qos, Topic: topic, Payload: []byte(payload), Retain: retained,
			}); err != nil {
				if ctx.Err() != nil {
					return
				}
				fmt.Fprintf(os.Stderr, "[mqttprobe] publish error: %v\n", err)
				continue
			}
			fmt.Printf("[mqttprobe] #%d → %s (%d bytes, qos=%d retain=%t)\n",
				count, topic, len(payload), qos, retained)
		}
	}
}

// === v3.1.1（paho.mqtt.golang）===

func runV311(ctx context.Context, brokerURL *url.URL, clientID, username, password, topic string,
	keepalive int, qos byte, retained, subscribe bool, timeout time.Duration) {

	opts := pahov311.NewClientOptions().
		AddBroker(brokerURL.String()).
		SetClientID(clientID).
		SetUsername(username).
		SetPassword(password).
		SetKeepAlive(time.Duration(keepalive) * time.Second).
		SetConnectTimeout(timeout).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(2 * time.Second).
		SetOrderMatters(false).
		SetProtocolVersion(4) // 4 = MQTT 3.1.1

	opts.OnConnect = func(_ pahov311.Client) {
		fmt.Println("[mqttprobe] connected ✓ (v3.1.1)")
	}
	opts.OnConnectionLost = func(_ pahov311.Client, err error) {
		fmt.Fprintf(os.Stderr, "[mqttprobe] connection lost: %v\n", err)
	}

	client := pahov311.NewClient(opts)

	// 阻塞首次连接（受 ctx 控制）
	type result struct{ err error }
	resCh := make(chan result, 1)
	go func() {
		tok := client.Connect()
		tok.Wait()
		resCh <- result{tok.Error()}
	}()
	select {
	case r := <-resCh:
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "[mqttprobe] connect 失败: %v\n", r.err)
			os.Exit(2)
		}
	case <-time.After(timeout):
		client.Disconnect(0)
		fmt.Fprintf(os.Stderr, "[mqttprobe] connect 超时 %v\n", timeout)
		os.Exit(2)
	case <-ctx.Done():
		client.Disconnect(0)
		fmt.Fprintf(os.Stderr, "[mqttprobe] ctx 取消\n")
		os.Exit(2)
	}

	defer client.Disconnect(2000)

	if subscribe {
		if tok := client.Subscribe(topic, qos, func(_ pahov311.Client, m pahov311.Message) {
			fmt.Printf("[mqttprobe] received topic=%s body=%s retain=%t\n",
				m.Topic(), string(m.Payload()), m.Retained())
		}); !tok.Wait() || tok.Error() != nil {
			fmt.Fprintf(os.Stderr, "[mqttprobe] subscribe failed: %v\n", tok.Error())
			os.Exit(2)
		}
		fmt.Printf("[mqttprobe] subscribed to %s\n", topic)
		fmt.Println("[mqttprobe] waiting for messages (Ctrl+C to stop) ...")
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	count := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Println("[mqttprobe] exit")
			return
		case <-ticker.C:
			count++
			payload := fmt.Sprintf(`{"ts":"%s","seq":%d,"hello":"mqttprobe","proto":"v3.1.1"}`,
				time.Now().UTC().Format(time.RFC3339Nano), count)
			tok := client.Publish(topic, qos, retained, payload)
			if !tok.Wait() {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if err := tok.Error(); err != nil {
				fmt.Fprintf(os.Stderr, "[mqttprobe] publish error: %v\n", err)
				continue
			}
			fmt.Printf("[mqttprobe] #%d → %s (%d bytes, qos=%d retain=%t)\n",
				count, topic, len(payload), qos, retained)
		}
	}
}

// normalizeBrokerURL 复刻 target/mqtt.go 的归一化（探针独立运行，避免循环 import）。
func normalizeBrokerURL(addr string) (*url.URL, error) {
	a := addr
	if a == "" {
		return nil, fmt.Errorf("broker 地址为空")
	}
	if !strings.Contains(a, "://") {
		a = "mqtt://" + a
	}
	if strings.HasPrefix(a, "tcp://") {
		a = "mqtt://" + strings.TrimPrefix(a, "tcp://")
	}
	if strings.HasPrefix(a, "ssl://") {
		a = "tls://" + strings.TrimPrefix(a, "ssl://")
	}
	u, err := url.Parse(a)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("broker 缺少 host: %s", a)
	}
	return u, nil
}
