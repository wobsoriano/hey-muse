// This file is a modified port of linux/src/musegadget/network.py from Meta's Muse Gadget SDK,
// Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0.

package pairing

import (
	"context"
	"errors"
	"net"
	"time"
)

// WiFiNetwork is one entry of the scan the phone asks for.
type WiFiNetwork struct {
	SSID   string `json:"ssid"`
	RSSI   int    `json:"rssi"`
	Secure bool   `json:"secure"`
}

// Network is how the device gets online during setup. The apps always scan and always send an SSID
// back, so a device has to answer both even when it has nothing to join. Scan and Join run off the
// Controller's loop and may take a while; Online is asked on it and should not.
type Network interface {
	// Online reports whether the device can reach Muse now.
	Online(ctx context.Context) bool
	// Scan lists the networks to offer the phone.
	Scan(ctx context.Context) ([]WiFiNetwork, error)
	// Join gets the device online with what the phone chose, and returns nil once it is.
	Join(ctx context.Context, ssid, password string) error
}

// ErrOffline is AlreadyOnline's answer when the device is not online after all.
var ErrOffline = errors.New("pairing: the device is not online")

// CurrentConnectionLabel names the offered network when the device does not know its SSID.
const CurrentConnectionLabel = "Use current connection"

const (
	apiHost       = "api.muse.ai"
	onlineTimeout = 5 * time.Second
)

// AlreadyOnline is the Network of a device that was put on Wi-Fi some other way before setup, which is
// how the Linux SDK works. It offers exactly one network, standing for the connection it has, marked
// open so the apps skip the password field, and ignores the Wi-Fi fields that come back.
type AlreadyOnline struct {
	// Reachable reports whether Muse can be reached. Nil tries a TCP connection to the Muse API.
	Reachable func(ctx context.Context) bool
	// SSID is the network the device is on, so the app can preselect it when the phone is on the same
	// one. Nil or an empty answer offers CurrentConnectionLabel.
	SSID func() string
}

func (n AlreadyOnline) Online(ctx context.Context) bool {
	if n.Reachable != nil {
		return n.Reachable(ctx)
	}
	d := net.Dialer{Timeout: onlineTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(apiHost, "443"))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (n AlreadyOnline) Scan(ctx context.Context) ([]WiFiNetwork, error) {
	if !n.Online(ctx) {
		return nil, nil
	}
	ssid := CurrentConnectionLabel
	if n.SSID != nil {
		if s := n.SSID(); s != "" {
			ssid = s
		}
	}
	return []WiFiNetwork{{SSID: ssid, RSSI: -40, Secure: false}}, nil
}

func (n AlreadyOnline) Join(ctx context.Context, _, _ string) error {
	if !n.Online(ctx) {
		return ErrOffline
	}
	return nil
}
