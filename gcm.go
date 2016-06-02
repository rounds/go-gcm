// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package gcm provides send and receive GCM functionality.
package gcm

import (
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/jpillora/backoff"
)

var (
	// Default Min and Max delay for backoff.
	DefaultMinBackoff   = 1 * time.Second
	DefaultMaxBackoff   = 10 * time.Second
	DefaultPingInterval = 20 * time.Second
	DefaultPingTimeout  = 15 * time.Second
)

// The data payload of a GCM message.
type Data map[string]interface{}

// The notification payload of a GCM message.
type Notification struct {
	Title        string `json:"title,omitempty"`
	Body         string `json:"body,omitempty"`
	Icon         string `json:"icon,omitempty"`
	Sound        string `json:"sound,omitempty"`
	Badge        string `json:"badge,omitempty"`
	Tag          string `json:"tag,omitempty"`
	Color        string `json:"color,omitempty"`
	ClickAction  string `json:"click_action,omitempty"`
	BodyLocKey   string `json:"body_loc_key,omitempty"`
	BodyLocArgs  string `json:"body_loc_args,omitempty"`
	TitleLocArgs string `json:"title_loc_args,omitempty"`
	TitleLocKey  string `json:"title_loc_key,omitempty"`
}

// Client is a container for http and xmpp GCM clients.
type Client struct {
	Debug      bool
	senderID   string
	apiKey     string
	mh         MessageHandler
	xmppClient *xmppGcmClient
	httpClient *httpGcmClient
	sandbox    bool
	debug      bool
}

// NewClient creates a new GCM client for this senderID.
func NewClient(isSandbox bool, senderID string, apiKey string, h MessageHandler, debug bool) (*Client, error) {
	c := &Client{
		senderID: senderID,
		apiKey:   apiKey,
		mh:       h,
		debug:    debug,
		sandbox:  isSandbox,
	}

	xm, err := connectXmpp(isSandbox, senderID, apiKey, c.onCCSMessage, debug)
	if err != nil {
		return nil, err
	}
	c.xmppClient = xm
	c.httpClient = newHttpGcmClient(apiKey, debug)

	// Ping periodically and indentify xmpp disconnect.
	go c.monitorConnection()

	log.WithField("sender id", senderID).Debug("gcm xmpp client created")
	return c, nil
}

// Send a message using the HTTP GCM connection server.
func (c *Client) SendHttp(m HttpMessage) (*HttpResponse, error) {
	b := newExponentialBackoff()
	return c.httpClient.sendHttp(m, b)
}

// SendXmpp sends a message using the XMPP GCM connection server.
func (c *Client) SendXmpp(m XmppMessage) (string, int, error) {
	return c.xmppClient.send(m)
}

// Close will stop and close the corresponding client.
func (c *Client) Close() error {
	c.xmppClient.gracefulClose()
	return nil
}

// Monitors the connection by periodic ping. When ping fails the xmpp client is replaced.
func (c *Client) monitorConnection() {
	for {
		if err := c.xmppClient.pingPeriodically(DefaultPingTimeout, DefaultPingInterval); err == nil {
			// Closed.
			break
		}
		log.Debug("gcm xmpp ping timed out, creating new xmpp client")
		if err := c.replaceXmppClient(true); err != nil {
			log.WithField("error", err).Error("error replacing xmpp client")
			time.Sleep(DefaultPingInterval)
		}
	}
}

// Replaces active xmpp client and closes the old one.
func (c *Client) replaceXmppClient(closeOld bool) error {
	newc, err := connectXmpp(c.sandbox, c.senderID, c.apiKey, c.onCCSMessage, c.debug)
	if err != nil {
		log.WithField("error", err).Error("error creating xmpp client")
		return err
	}
	oldc := c.xmppClient
	c.xmppClient = newc
	go c.monitorConnection()
	if closeOld {
		oldc.gracefulClose()
	}
	return nil
}

// CCS upstream message callback.
// Tries to handle what it can here, before bubbling up.
func (c *Client) onCCSMessage(cm CcsMessage) error {
	switch {
	case cm.MessageType == CCSNack && cm.Error == "CONNECTION_DRAINING",
		cm.MessageType == CCSControl && cm.ControlType == "CONNECTION_DRAINING":
		// Replace active xmpp client when server starts draining the current connection.
		log.WithField("ccs message", cm).Warn("connection draining, replacing xmpp client")
		if err := c.replaceXmppClient(false); err != nil {
			log.WithField("error", err).Error("error replacing xmpp client")
		}
		if cm.MessageType == CCSControl {
			// Don't bubble up, it's not a reply error.
			return nil
		}
	}
	// Bubble up.
	return c.mh(cm)
}

// Creates a new xmpp client, connects to the server and starts listening.
func connectXmpp(isSandbox bool, senderID string, apiKey string, h MessageHandler, debug bool) (*xmppGcmClient, error) {
	x, err := newXmppGcmClient(isSandbox, senderID, apiKey, debug)
	if err != nil {
		return nil, err
	}

	// Start listening on this connection.
	go func() {
		if err := x.listen(h); err != nil {
			// Pass the error upstream.
			//c.cerr <- err
			log.WithField("error", err).Error("gcm listen")
		}
		log.Debug("gcm listen finished")
	}()

	return x, nil
}

// Implementation of backoff provider using exponential backoff.
type exponentialBackoff struct {
	b            backoff.Backoff
	currentDelay time.Duration
}

// Factory method for exponential backoff, uses default values for Min and Max and
// adds Jitter.
func newExponentialBackoff() *exponentialBackoff {
	b := &backoff.Backoff{
		Min:    DefaultMinBackoff,
		Max:    DefaultMaxBackoff,
		Jitter: true,
	}
	return &exponentialBackoff{b: *b, currentDelay: b.Duration()}
}

// Returns true if not over the retries limit
func (eb exponentialBackoff) sendAnother() bool {
	return eb.currentDelay <= eb.b.Max
}

// Set the minumim delay for backoff
func (eb *exponentialBackoff) setMin(min time.Duration) {
	eb.b.Min = min
	if (eb.currentDelay) < min {
		eb.currentDelay = min
	}
}

// Wait for the current value of backoff
func (eb exponentialBackoff) wait() {
	time.Sleep(eb.currentDelay)
	eb.currentDelay = eb.b.Duration()
}
