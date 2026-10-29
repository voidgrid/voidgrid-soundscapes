package hamqtt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// BrokerOptions say how to reach the broker.
type BrokerOptions struct {
	URL      string // tcp://host:1883, ssl://host:8883
	User     string
	Password string
}

const (
	qos         = 1
	ioTimeout   = 10 * time.Second
	connectWarn = 15 * time.Second
)

// Dial connects to the broker and keeps the connection up: it retries until it succeeds and reconnects when
// it is lost. c.Connected runs after every (re)connection and c.Disconnected when one is lost. The last will
// marks the device offline if this program dies. It returns at once; call the returned function to disconnect
// cleanly (which marks the device offline).
func Dial(b BrokerOptions, c *Controller, log *slog.Logger) (stop func()) {
	mqtt.ERROR = pahoLog{log, slog.LevelError}
	mqtt.CRITICAL = pahoLog{log, slog.LevelError}
	mqtt.WARN = pahoLog{log, slog.LevelWarn}

	var client mqtt.Client
	opts := mqtt.NewClientOptions().
		AddBroker(b.URL).
		SetClientID(c.ClientID()).
		SetKeepAlive(30*time.Second).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5*time.Second).
		SetMaxReconnectInterval(30*time.Second).
		SetConnectTimeout(ioTimeout).
		SetWriteTimeout(ioTimeout).
		SetWill(c.StatusTopic(), offline, qos, true).
		SetOnConnectHandler(func(mqtt.Client) {
			log.Info("connected to the MQTT broker", "broker", b.URL)
			c.Connected(&pahoConn{client: client})
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			log.Warn("lost the MQTT connection; reconnecting", "err", err)
			c.Disconnected()
		})
	if b.User != "" {
		opts.SetUsername(b.User).SetPassword(b.Password)
	}
	client = mqtt.NewClient(opts)
	tok := client.Connect() // with ConnectRetry the attempts continue in the background
	go func() {
		if !tok.WaitTimeout(connectWarn) {
			log.Warn("the MQTT broker is not reachable yet; still trying", "broker", b.URL)
		}
	}()
	return func() {
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn != nil {
			_ = conn.Publish(c.StatusTopic(), offline, true) // a clean disconnect does not send the last will
		}
		client.Disconnect(500)
	}
}

type pahoConn struct{ client mqtt.Client }

func (p *pahoConn) Publish(topic, payload string, retain bool) error {
	t := p.client.Publish(topic, qos, retain, payload)
	if !t.WaitTimeout(ioTimeout) {
		return errors.New("timed out")
	}
	return t.Error()
}

func (p *pahoConn) Subscribe(topic string, handler func(topic, payload string)) error {
	t := p.client.Subscribe(topic, qos, func(_ mqtt.Client, m mqtt.Message) { handler(m.Topic(), string(m.Payload())) })
	if !t.WaitTimeout(ioTimeout) {
		return errors.New("timed out")
	}
	return t.Error()
}

// pahoLog sends the library's own messages to our logger.
type pahoLog struct {
	log   *slog.Logger
	level slog.Level
}

func (l pahoLog) Println(v ...interface{}) { l.emit(fmt.Sprint(v...)) }

func (l pahoLog) Printf(format string, v ...interface{}) { l.emit(fmt.Sprintf(format, v...)) }

// emit logs a message from the library. Its in-memory message store reports "wiped" and "message not found"
// as warnings on every connect; they are not problems, so they are only shown when debugging.
func (l pahoLog) emit(msg string) {
	level := l.level
	if strings.Contains(msg, "[store]") {
		level = slog.LevelDebug
	}
	l.log.Log(context.Background(), level, "mqtt: "+msg)
}
