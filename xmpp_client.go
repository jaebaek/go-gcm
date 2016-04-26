// Package gcm provides send and receive GCM functionality.
package gcm

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/mattn/go-xmpp"
	"github.com/pborman/uuid"
)

const (
	CCSAck      = "ack"
	CCSNack     = "nack"
	CCSControl  = "control"
	CCSReceipt  = "receipt"
	xmppHost    = "gcm.googleapis.com"
	xmppPort    = "5235"
	xmppAddress = xmppHost + ":" + xmppPort
	// For ccs the min for exponential backoff has to be 1 sec
	ccsMinBackoff = 1 * time.Second
)

var (
	retryableErrors = map[string]bool{
		"Unavailable":           true,
		"SERVICE_UNAVAILABLE":   true,
		"InternalServerError":   true,
		"INTERNAL_SERVER_ERROR": true,
		// TODO(silvano): should we backoff with the same strategy on
		// DeviceMessageRateExceeded and TopicsMessageRateExceeded.
	}
)

// A GCM Xmpp message.
type XmppMessage struct {
	To                       string        `json:"to,omitempty"`
	MessageId                string        `json:"message_id"`
	MessageType              string        `json:"message_type,omitempty"`
	CollapseKey              string        `json:"collapse_key,omitempty"`
	Priority                 string        `json:"priority,omitempty"`
	ContentAvailable         bool          `json:"content_available,omitempty"`
	DelayWhileIdle           bool          `json:"delay_while_idle,omitempty"`
	TimeToLive               uint          `json:"time_to_live,omitempty"`
	DeliveryReceiptRequested bool          `json:"delivery_receipt_requested,omitempty"`
	DryRun                   bool          `json:"dry_run,omitempty"`
	Data                     Data          `json:"data,omitempty"`
	Notification             *Notification `json:"notification,omitempty"`
}

// CcsMessage is an Xmpp message sent from CCS.
type CcsMessage struct {
	From             string `json:"from, omitempty"`
	MessageId        string `json:"message_id, omitempty"`
	MessageType      string `json:"message_type, omitempty"`
	RegistrationId   string `json:"registration_id,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	Category         string `json:"category, omitempty"`
	Data             Data   `json:"data,omitempty"`
	ControlType      string `json:"control_type,omitempty"`
}

// MessageHandler is the type for a function that handles a CCS message.
// The CCS message can be an upstream message (device to server) or a
// message from CCS (e.g. a delivery receipt).
type MessageHandler func(cm CcsMessage) error

// xmppClient is an interface to stub the xmpp client in tests.
type xmppClient interface {
	listen(h MessageHandler, stop chan<- bool) error
	send(m XmppMessage) (int, error)
}

// xmppGcmClient is a client for the Gcm Xmpp Connection Server (CCS).
type xmppGcmClient struct {
	sync.RWMutex
	XmppClient xmpp.Client
	messages   struct {
		sync.RWMutex
		m map[string]*messageLogEntry
	}
	pongs      chan struct{}
	senderID   string
	isClosed   bool
	destructor sync.Once
	debug      bool
}

// An entry in the messages log, used to keep track of messages pending ack and
// retries for failed messages.
type messageLogEntry struct {
	body    *XmppMessage
	backoff *exponentialBackoff
}

// Factory method for xmppGcmClient, to minimize the number of clients to one per sender id.
// TODO(silvano): this could be revised, taking into account that we cannot have more than 1000
// connections per senderId.
func newXmppGcmClient(senderID string, apiKey string, debug bool) (*xmppGcmClient, error) {
	nc, err := xmpp.NewClient(xmppAddress, xmppUser(senderID), apiKey, debug)
	if err != nil {
		return nil, fmt.Errorf("error connecting xmpp client: %v", err)
	}

	xc := &xmppGcmClient{
		XmppClient: *nc,
		messages: struct {
			sync.RWMutex
			m map[string]*messageLogEntry
		}{
			m: make(map[string]*messageLogEntry),
		},
		senderID: senderID,
		pongs:    make(chan struct{}),
		debug:    debug,
	}

	return xc, nil
}

// ping sends a c2s ping message and blocks until s2c pong is received.
//
// Returns error if timeout time passes before pong.
func (c *xmppGcmClient) ping(timeout time.Duration) error {
	if err := c.XmppClient.PingC2S("", xmppHost); err != nil {
		return err
	}
	select {
	case <-c.pongs:
		//if pongID == pingID {
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("xmpp pong timed out after %s", timeout.String())
	}
}

// pingPeriodically sends periodic pings. If pong is received, the timeout
// interval is reset, otherwise client close is triggered.
func (c *xmppGcmClient) pingPeriodically(timeout, interval time.Duration) error {
	t := time.NewTimer(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if err := c.ping(timeout); err != nil {
				c.RLock()
				defer c.RUnlock()
				if c.isClosed {
					return nil
				}
				return err
			}
			t.Reset(interval)
		}
	}
	return nil
}

func (c *xmppGcmClient) waitAllDone() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			c.messages.RLock()
			nm := len(c.messages.m)
			c.messages.RUnlock()
			if nm == 0 {
				close(ch)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return ch
}

func (c *xmppGcmClient) gracefulClose() {
	c.destructor.Do(func() {
		log.Debug("xmppGcmClient graceful close started")
		c.Lock()
		if c.isClosed {
			c.Unlock()
			return
		}
		c.isClosed = true
		c.Unlock()

		// Wait until all is done, or timed out.
		select {
		case <-c.waitAllDone():
		case <-time.After(DefaultPingTimeout):
			log.Debug("xmpp taking a while to close, so giving up")
		}

		c.XmppClient.Close()
	})
}

// xmppGcmClient implementation of listening for messages from CCS; the messages can be
// acks or nacks for messages sent through XMPP, control messages, upstream messages.
func (c *xmppGcmClient) listen(h MessageHandler) error {
	for {
		stanza, err := c.XmppClient.Recv()
		if err != nil {
			c.RLock()
			defer c.RUnlock()
			if c.isClosed {
				// Client is closed, return without error.
				break
			}
			// This is likely fatal, so return.
			return fmt.Errorf("error on Recv: %v", err)
		}
		switch v := stanza.(type) {
		case xmpp.Chat:
		case xmpp.IQ:
			if v.Type == "result" && v.ID == "c2s1" {
				c.pongs <- struct{}{}
			}
			continue
		case xmpp.Presence:
			continue
		}

		v := stanza.(xmpp.Chat)
		switch v.Type {
		case "":
			cm := &CcsMessage{}
			err = json.Unmarshal([]byte(v.Other[0]), cm)
			if err != nil {
				log.WithField("error", err).Error("unmarshaling ccs message")
				continue
			}
			switch cm.MessageType {
			case CCSAck:
				c.messages.Lock()
				// ack for a sent message, delete it from log.
				if _, ok := c.messages.m[cm.MessageId]; ok {
					go h(*cm)
					delete(c.messages.m, cm.MessageId)
				}
				c.messages.Unlock()
			case CCSNack:
				// nack for a sent message, retry if retryable error, bubble up otherwise.
				if retryableErrors[cm.Error] {
					c.retryMessage(*cm, h)
				} else {
					c.messages.Lock()
					if _, ok := c.messages.m[cm.MessageId]; ok {
						go h(*cm)
						delete(c.messages.m, cm.MessageId)
					}
					c.messages.Unlock()
				}
			case CCSControl:
				log.WithField("ccs message", cm).Warn("control message")
				go h(*cm)
			default:
				log.WithField("ccs message", cm).Warn("unknown ccs message")
			}
		case "normal":
			cm := &CcsMessage{}
			err = json.Unmarshal([]byte(v.Other[0]), cm)
			if err != nil {
				log.WithField("error", err).Error("unmarshaling ccs message")
				continue
			}
			switch cm.MessageType {
			case CCSReceipt:
				if c.debug {
					log.WithField("ccs message", cm).Debug("message receipt")
				}
				// Receipt message: send ack and pass to listener.
				ack := XmppMessage{To: cm.From, MessageId: cm.MessageId, MessageType: CCSAck}
				c.send(ack)
				go h(*cm)
			default:
				log.WithField("ccs message", cm).Warn("uknown upstream message")
				// Upstream message: send ack and pass to listener.
				ack := XmppMessage{To: cm.From, MessageId: cm.MessageId, MessageType: CCSAck}
				c.send(ack)
				go h(*cm)
			}
		case "error":
			log.WithField("stanza", v).Warn("error response")
		default:
			log.WithField("stanza", v).Warn("error message type")
		}
	}
	return nil
}

//TODO(silvano): add flow control (max 100 pending messages at one time)
// xmppGcmClient implementation to send a message through Gcm Xmpp server (ccs).
func (c *xmppGcmClient) send(m XmppMessage) (string, int, error) {
	if m.MessageId == "" {
		m.MessageId = uuid.New()
	}

	stanza := `<message id=""><gcm xmlns="google:mobile:data">%v</gcm></message>`
	body, err := json.Marshal(m)
	if err != nil {
		return m.MessageId, 0, fmt.Errorf("could not unmarshal body of xmpp message>%v", err)
	}
	bs := string(body)

	payload := fmt.Sprintf(stanza, bs)

	if c.debug {
		log.WithField("xmpp payload", payload).Debug("sending xmpp")
	}

	// For Acks, just send the message.
	if m.MessageType == CCSAck {
		// Serialize wire access for thread safety.
		c.Lock()
		bytes, err := c.XmppClient.SendOrg(payload)
		c.Unlock()
		return m.MessageId, bytes, err
	}

	// For payload messages, keep them until Ack/Nack is received.
	c.messages.Lock()
	if _, ok := c.messages.m[m.MessageId]; !ok {
		b := newExponentialBackoff()
		if b.b.Min < ccsMinBackoff {
			b.setMin(ccsMinBackoff)
		}
		c.messages.m[m.MessageId] = &messageLogEntry{body: &m, backoff: b}
	}
	c.messages.Unlock()

	// Serialize wire access for thread safety.
	c.Lock()
	bytes, err := c.XmppClient.SendOrg(payload)
	c.Unlock()

	if err != nil {
		c.messages.Lock()
		delete(c.messages.m, m.MessageId)
		c.messages.Unlock()
	}
	return m.MessageId, bytes, err
}

// Retry sending an xmpp message with exponential backoff; if over limit, bubble up the failed message.
func (c *xmppGcmClient) retryMessage(cm CcsMessage, h MessageHandler) {
	c.messages.RLock()
	defer c.messages.RUnlock()
	if me, ok := c.messages.m[cm.MessageId]; ok {
		if me.backoff.sendAnother() {
			go func(m *messageLogEntry) {
				m.backoff.wait()
				c.send(*m.body)
			}(me)
		} else {
			log.WithField("xmpp payload", me).Warn("exponential backoff failed over limit")
			go h(cm)
		}
	}
}

// xmppUser generates an xmpp username from a sender ID.
func xmppUser(senderId string) string {
	return senderId + "@" + xmppHost
}
