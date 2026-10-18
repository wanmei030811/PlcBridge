package main

import (
	"fmt"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func main() {
	opts := mqtt.NewClientOptions().
		AddBroker("tcp://127.0.0.1:1883").
		SetClientID(fmt.Sprintf("verify-bool-%d", time.Now().UnixNano())).
		SetConnectTimeout(5 * time.Second)

	c := mqtt.NewClient(opts)
	if t := c.Connect(); t.Wait() && t.Error() != nil {
		fmt.Println("connect err:", t.Error())
		os.Exit(1)
	}
	fmt.Println("connected, subscribing sss/# for 6s...")
	c.Subscribe("sss/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		fmt.Printf("[%s] %s\n", m.Topic(), string(m.Payload()))
	})
	time.Sleep(6 * time.Second)
}
