/*
* Copyright (C) 2015 Alexey Gladkov <gladkov.alexey@gmail.com>
*
* This file is covered by the GNU General Public License,
* which should be included with kafka-http-proxy as the file COPYING.
 */

package main

import (
	"time"
)

// CfgDuration is a Duration wrapper for Config.
type CfgDuration struct {
	time.Duration
}

// UnmarshalText is a wrapper.
func (d *CfgDuration) UnmarshalText(data []byte) (err error) {
	d.Duration, err = time.ParseDuration(string(data))
	return
}

// Config is a main config structure
type Config struct {
	Global struct {
		Address    string
		Logfile    string
		Pidfile    string
		Verbose    bool
		GoMaxProcs int
		MaxConns   int64
	}
	Kafka struct {
		Broker []string
	}
	Broker struct {
		NumConns            int64
		LeaderRetryLimit    int
		LeaderRetryWait     CfgDuration
		DialTimeout         CfgDuration
		ReconnectPeriod     CfgDuration
		GetOffsetsTimeout   CfgDuration
		MetadataCachePeriod CfgDuration
		GetMetadataTimeout  CfgDuration
	}
	Producer struct {
		RequestTimeout     CfgDuration
		RetryLimit         int
		RetryWait          CfgDuration
		SendMessageTimeout CfgDuration
	}
	Consumer struct {
		RequestTimeout    CfgDuration
		RetryLimit        int
		RetryWait         CfgDuration
		RetryErrLimit     int
		RetryErrWait      CfgDuration
		GetMessageTimeout CfgDuration
		MinFetchSize      int32
		MaxFetchSize      int32
		DefaultFetchSize  int32
	}
	OffsetCoordinator struct {
		RetryErrLimit       int
		RetryErrWait        CfgDuration
		CommitOffsetTimeout CfgDuration
		FetchOffsetTimeout  CfgDuration
	}
	Logging struct {
		DisableColors    bool
		DisableTimestamp bool
		FullTimestamp    bool
		DisableSorting   bool
	}
}

// SetDefaults applies default values to config structure.
func (c *Config) SetDefaults() {
	c.Global.Verbose = false
	c.Global.GoMaxProcs = 0
	c.Global.MaxConns = 1000000
	c.Global.Logfile = "/var/log/kafka-http-proxy.log"
	c.Global.Pidfile = "/run/kafka-http-proxy.pid"

	c.Broker.NumConns = 100
	c.Broker.DialTimeout.Duration = 500 * time.Millisecond
	c.Broker.LeaderRetryLimit = 2
	c.Broker.LeaderRetryWait.Duration = 500 * time.Millisecond
	c.Broker.ReconnectPeriod.Duration = 15 * time.Second
	c.Broker.MetadataCachePeriod.Duration = 3 * time.Second
	c.Broker.GetMetadataTimeout.Duration = 1 * time.Second
	c.Broker.GetOffsetsTimeout.Duration = 10 * time.Second

	c.Producer.RequestTimeout.Duration = 5 * time.Second
	c.Producer.RetryLimit = 2
	c.Producer.RetryWait.Duration = 200 * time.Millisecond
	c.Producer.SendMessageTimeout.Duration = 15 * time.Second

	c.Consumer.RequestTimeout.Duration = 50 * time.Millisecond
	c.Consumer.RetryLimit = 2
	c.Consumer.RetryWait.Duration = 50 * time.Millisecond
	c.Consumer.RetryErrLimit = 2
	c.Consumer.RetryErrWait.Duration = 50 * time.Millisecond
	c.Consumer.GetMessageTimeout.Duration = 15 * time.Second
	c.Consumer.MinFetchSize = 1
	c.Consumer.MaxFetchSize = 4194304
	c.Consumer.DefaultFetchSize = 524288

	c.OffsetCoordinator.RetryErrLimit = 2
	c.OffsetCoordinator.RetryErrWait.Duration = 200 * time.Millisecond
	c.OffsetCoordinator.CommitOffsetTimeout.Duration = 15 * time.Second
	c.OffsetCoordinator.FetchOffsetTimeout.Duration = 15 * time.Second

	c.Logging.DisableColors = true
	c.Logging.DisableTimestamp = false
	c.Logging.FullTimestamp = true
	c.Logging.DisableSorting = true
}
