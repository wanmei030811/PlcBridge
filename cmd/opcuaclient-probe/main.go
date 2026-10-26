// opcuaclient-probe —— 用 gopcua/opcua 直接连 opc.tcp://host:port 探一个节点，验证 OPC UA server 可达。
//
// 用法：
//
//	go run ./cmd/opcuaclient-probe
//	go run ./cmd/opcuaclient-probe -endpoint opc.tcp://127.0.0.1:4840 -node "ns=0;i=2258"
//	go run ./cmd/opcuaclient-probe -subscribe -node "ns=2;s=Demo.Dynamic.Scalar.Float"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
)

func main() {
	endpoint := flag.String("endpoint", "opc.tcp://127.0.0.1:4840", "OPC UA Endpoint URL")
	node := flag.String("node", "ns=0;i=2258", "NodeID to read (e.g. 'ns=2;s=Demo.Dynamic.Scalar.Float')")
	subscribe := flag.Bool("subscribe", false, "Use subscription (MonitoredItem) instead of polling read")
	timeout := flag.Duration("timeout", 5*time.Second, "Connect timeout")
	publishInterval := flag.Duration("publish-interval", 1*time.Second, "Subscription publish interval")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 监听 Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n[opcuaclient-probe] interrupted")
		cancel()
	}()

	fmt.Printf("[opcuaclient-probe] resolving endpoint %s ...\n", *endpoint)
	eps, err := opcua.GetEndpoints(ctx, *endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[opcuaclient-probe] GetEndpoints FAILED: %v\n", err)
		os.Exit(2)
	}
	ep, err := opcua.SelectEndpoint(eps, "None", ua.MessageSecurityModeNone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[opcuaclient-probe] SelectEndpoint FAILED: %v\n", err)
		os.Exit(2)
	}
	ep.EndpointURL = *endpoint

	fmt.Printf("[opcuaclient-probe] connecting ... security=%s mode=%s\n", ep.SecurityPolicyURI, ep.SecurityMode)
	opts := []opcua.Option{
		opcua.SecurityMode(ua.MessageSecurityModeNone),
		opcua.SecurityPolicy(ua.SecurityPolicyURINone),
		opcua.AuthAnonymous(),
		opcua.SecurityFromEndpoint(ep, ua.UserTokenTypeAnonymous),
		opcua.DialTimeout(*timeout),
	}
	cli, err := opcua.NewClient(*endpoint, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[opcuaclient-probe] NewClient FAILED: %v\n", err)
		os.Exit(2)
	}
	connectCtx, connectCancel := context.WithTimeout(ctx, *timeout)
	defer connectCancel()
	if err := cli.Connect(connectCtx); err != nil {
		fmt.Fprintf(os.Stderr, "[opcuaclient-probe] Connect FAILED: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("[opcuaclient-probe] connected ✓")
	defer cli.Close(context.Background())

	nid, err := ua.ParseNodeID(*node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[opcuaclient-probe] invalid node id %q: %v\n", *node, err)
		os.Exit(2)
	}

	if !*subscribe {
		req := &ua.ReadRequest{
			MaxAge: 2000,
			NodesToRead: []*ua.ReadValueID{
				{NodeID: nid, AttributeID: ua.AttributeIDValue},
			},
			TimestampsToReturn: ua.TimestampsToReturnBoth,
		}
		resp, err := cli.Read(ctx, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[opcuaclient-probe] Read FAILED: %v\n", err)
			os.Exit(3)
		}
		if resp == nil || len(resp.Results) == 0 {
			fmt.Fprintln(os.Stderr, "[opcuaclient-probe] Read empty response")
			os.Exit(3)
		}
		r := resp.Results[0]
		if r.Status != ua.StatusOK {
			fmt.Fprintf(os.Stderr, "[opcuaclient-probe] Read status not OK: %v\n", r.Status)
			os.Exit(3)
		}
		fmt.Printf("[opcuaclient-probe] %s = %v (status=%v)\n", *node, r.Value.Value(), r.Status)
		return
	}

	// 订阅模式
	notifyCh := make(chan *opcua.PublishNotificationData, 16)
	sub, err := cli.Subscribe(ctx, &opcua.SubscriptionParameters{
		Interval: *publishInterval,
	}, notifyCh)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[opcuaclient-probe] Subscribe FAILED: %v\n", err)
		os.Exit(3)
	}
	defer sub.Cancel(ctx)
	fmt.Printf("[opcuaclient-probe] subscription id=%v, interval=%s\n", sub.SubscriptionID, *publishInterval)

	handle := uint32(1)
	mreq := opcua.NewMonitoredItemCreateRequestWithDefaults(nid, ua.AttributeIDValue, handle)
	res, err := sub.Monitor(ctx, ua.TimestampsToReturnBoth, mreq)
	if err != nil || (len(res.Results) > 0 && res.Results[0].StatusCode != ua.StatusOK) {
		st := ua.StatusCode(0)
		if len(res.Results) > 0 {
			st = res.Results[0].StatusCode
		}
		fmt.Fprintf(os.Stderr, "[opcuaclient-probe] Monitor FAILED: %v status=%v\n", err, st)
		os.Exit(3)
	}
	fmt.Println("[opcuaclient-probe] monitored item created ✓ (Ctrl+C to stop)")

	for {
		select {
		case <-ctx.Done():
			fmt.Println("[opcuaclient-probe] exit")
			return
		case res, ok := <-notifyCh:
			if !ok {
				return
			}
			if res == nil || res.Error != nil {
				continue
			}
			dcn, ok := res.Value.(*ua.DataChangeNotification)
			if !ok {
				continue
			}
			for _, item := range dcn.MonitoredItems {
				val := "<nil>"
				if item.Value != nil && item.Value.Value != nil {
					val = fmt.Sprintf("%v", item.Value.Value.Value())
				}
				fmt.Printf("[opcuaclient-probe] %s = %s (status=%v, source_ts=%v)\n",
					*node, val, item.Value.Status, item.Value.SourceTimestamp)
			}
		}
	}
}
