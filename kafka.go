/*
* Copyright (C) 2015 Alexey Gladkov <gladkov.alexey@gmail.com>
*
* This file is covered by the GNU General Public License,
* which should be included with kafka-http-proxy as the file COPYING.
 */

package main

import (
	"github.com/facebookgo/metrics"
	"github.com/optiopay/kafka"
	"github.com/optiopay/kafka/proto"

	log "github.com/Sirupsen/logrus"

	"fmt"
	"sync"
	"time"
)

var (
	// KafkaOffsetNewest is a wrapper over kafka.StartOffsetNewest
	KafkaOffsetNewest = kafka.StartOffsetNewest

	// KafkaOffsetOldest is a wrapper over kafka.StartOffsetOldest
	KafkaOffsetOldest = kafka.StartOffsetOldest

	// KafkaErrReplicaNotAvailable is a wrapper over proto.ErrReplicaNotAvailable
	KafkaErrReplicaNotAvailable = proto.ErrReplicaNotAvailable

	// KafkaErrUnknownTopicOrPartition is a wrapper over proto.ErrUnknownTopicOrPartition
	KafkaErrUnknownTopicOrPartition = proto.ErrUnknownTopicOrPartition

	// KafkaErrNoData is a wrapper over kafka.ErrNoData
	KafkaErrNoData = kafka.ErrNoData
)

const (
	_ = iota
	KhpErrorNoBrokers
	KhpErrorReadTimeout
	KhpErrorWriteTimeout
	KhpErrorOffsetCommitTimeout
	KhpErrorOffsetFetchTimeout
	KhpErrorConsumerClosed
	KhpErrorProducerClosed
	KhpErrorOffsetCoordinatorClosed
	KhpErrorMetadataReadTimeout
)

type kafkaLogger struct {
	subsys string
}

func (l *kafkaLogger) Debug(msg string, args ...interface{}) {
	e := log.NewEntry(log.StandardLogger())

	for i := 0; i < len(args); i += 2 {
		k := fmt.Sprintf("%+v", args[i])
		e = e.WithField(k, args[i+1])
	}

	e.Debugf("[%s] %s", l.subsys, msg)
}

func (l *kafkaLogger) Info(msg string, args ...interface{}) {
	e := log.NewEntry(log.StandardLogger())

	for i := 0; i < len(args); i += 2 {
		k := fmt.Sprintf("%+v", args[i])
		e = e.WithField(k, args[i+1])
	}

	e.Infof("[%s] %s", l.subsys, msg)
}

func (l *kafkaLogger) Warn(msg string, args ...interface{}) {
	e := log.NewEntry(log.StandardLogger())

	for i := 0; i < len(args); i += 2 {
		k := fmt.Sprintf("%+v", args[i])
		e = e.WithField(k, args[i+1])
	}

	e.Warningf("[%s] %s", l.subsys, msg)
}

func (l *kafkaLogger) Error(msg string, args ...interface{}) {
	e := log.NewEntry(log.StandardLogger())

	for i := 0; i < len(args); i += 2 {
		k := fmt.Sprintf("%+v", args[i])
		e = e.WithField(k, args[i+1])
	}

	e.Errorf("[%s] %s", l.subsys, msg)
}

// KhpError is our own errors
type KhpError struct {
	Errno   int
	message string
}

func (e KhpError) Error() string {
	return e.message
}

// KafkaClient is batch of brokers
type KafkaClient struct {
	GetMetadataTimeout  time.Duration
	MetadataCachePeriod time.Duration
	GetOffsetsTimeout   time.Duration
	ReconnectPeriod     time.Duration

	allBrokers    map[int64]*kafka.Broker
	deadBrokers   chan int64
	freeBrokers   chan int64
	stopReconnect chan struct{}

	cache struct {
		sync.RWMutex

		lastMetadata       *KafkaMetadata
		lastUpdateMetadata int64
	}

	Timings  map[string]metrics.Timer
	Counters map[string]metrics.Counter
}

// NewClient creates new KafkaClient
func NewClient(settings *Config) (*KafkaClient, error) {
	conf := kafka.NewBrokerConf("kafka-http-proxy")

	conf.Logger = &kafkaLogger{
		subsys: "kafka/broker",
	}

	conf.DialTimeout = settings.Broker.DialTimeout.Duration
	conf.LeaderRetryLimit = settings.Broker.LeaderRetryLimit
	conf.LeaderRetryWait = settings.Broker.LeaderRetryWait.Duration
	conf.AllowTopicCreation = settings.Broker.AllowTopicCreation

	log.Debug("Gona create broker pool = ", settings.Broker.NumConns)

	client := &KafkaClient{
		GetMetadataTimeout:  settings.Broker.GetMetadataTimeout.Duration,
		MetadataCachePeriod: settings.Broker.MetadataCachePeriod.Duration,
		GetOffsetsTimeout:   settings.Broker.GetOffsetsTimeout.Duration,
		ReconnectPeriod:     settings.Broker.ReconnectPeriod.Duration,
		Timings:             NewTimings([]string{"GetMetadata", "GetOffsets", "GetMessage", "SendMessage", "CommitOffset", "FetchOffset"}),
		Counters:            NewCounters([]string{"DeadBrokers", "FreeBrokers"}),
		allBrokers:          make(map[int64]*kafka.Broker),
		deadBrokers:         make(chan int64, settings.Broker.NumConns),
		freeBrokers:         make(chan int64, settings.Broker.NumConns),
		stopReconnect:       make(chan struct{}),
	}

	brokerID := int64(0)

	for brokerID < settings.Broker.NumConns {
		b, err := kafka.Dial(settings.Kafka.Broker, conf)
		if err != nil {
			_ = client.Close()
			return nil, err
		}

		client.allBrokers[brokerID] = b
		client.freeBroker(brokerID)
		brokerID++
	}

	if client.MetadataCachePeriod > 0 {
		go func() {
			for {
				select {
				case <-time.After(client.MetadataCachePeriod):
				case <-client.stopReconnect:
					return
				}

				meta, err := client.GetMetadata()
				if err != nil {
					conf.Logger.Error("Unable to fetch metadata", "err", err.Error())
					continue
				}

				client.cache.Lock()
				client.cache.lastMetadata = meta
				client.cache.lastUpdateMetadata = time.Now().UnixNano()
				client.cache.Unlock()

				conf.Logger.Info("Got new metadata by schedule")
			}
		}()
	}

	if client.ReconnectPeriod > 0 {
		go func() {
			for {
				select {
				case <-time.After(client.ReconnectPeriod):
					if id, goErr := client.getBroker(); goErr == nil {
						client.deadBroker(id)
					}
				case <-client.stopReconnect:
					return
				}
			}
		}()
	}

	go func() {
		var id int64

		for {
			select {
			case id = <-client.deadBrokers:
			case <-client.stopReconnect:
				return
			}
			client.Counters["DeadBrokers"].Dec(1)

			go func(id int64) {
				client.allBrokers[id].Close()
				for {
					b, goErr := kafka.Dial(settings.Kafka.Broker, conf)
					if goErr == nil {
						client.allBrokers[id] = b
						client.freeBroker(id)
						break
					}
					conf.Logger.Error("Unable to reconnect", "brokerID", id, "err", goErr.Error())
				}
				conf.Logger.Info("Connection was reset", "brokerID", id)
			}(id)
		}
	}()

	return client, nil
}

// Close closes all brokers.
func (k *KafkaClient) Close() error {
	close(k.stopReconnect)
	for _, broker := range k.allBrokers {
		broker.Close()
	}
	return nil
}

// Broker returns first availiable broker or error.
func (k *KafkaClient) getBroker() (int64, error) {
	select {
	case brokerID, ok := <-k.freeBrokers:
		if ok {
			k.Counters["FreeBrokers"].Dec(1)
			return brokerID, nil
		}
	default:
	}
	return 0, KhpError{
		Errno:   KhpErrorNoBrokers,
		message: "no brokers available",
	}
}

func (k *KafkaClient) freeBroker(brokerID int64) {
	k.freeBrokers <- brokerID
	k.Counters["FreeBrokers"].Inc(1)
}

func (k *KafkaClient) deadBroker(brokerID int64) {
	k.deadBrokers <- brokerID
	k.Counters["DeadBrokers"].Inc(1)
}

// GetOffsets returns oldest and newest offsets for partition.
func (k *KafkaClient) GetOffsets(topic string, partitionID int32) (int64, int64, error) {
	brokerID, err := k.getBroker()
	if err != nil {
		return 0, 0, err
	}

	defer k.Timings["GetOffsets"].Start().Stop()

	type offsetInfo struct {
		result  int64
		fetcher func(string, int32) (int64, error)
	}

	offsets := []offsetInfo{
		offsetInfo{0, k.allBrokers[brokerID].OffsetEarliest},
		offsetInfo{0, k.allBrokers[brokerID].OffsetLatest},
	}

	results := make(chan error, 2)
	timeout := make(chan struct{})

	if k.GetOffsetsTimeout > 0 {
		timer := time.AfterFunc(k.GetOffsetsTimeout, func() { close(timeout) })
		defer timer.Stop()
	}

	for i := range offsets {
		go func(i int) {
			var goErr error

			for retry := 0; retry < 2; retry++ {
				select {
				case <-timeout:
					return
				default:
				}

				offsets[i].result, goErr = offsets[i].fetcher(topic, partitionID)

				if goErr == nil {
					break
				}

				if _, ok := goErr.(*proto.KafkaError); ok {
					break
				}
			}
			results <- goErr
		}(i)
	}

	isTimeout := false

	for _ = range offsets {
		select {
		case err = <-results:
			if err != nil {
				break
			}
		case <-timeout:
			isTimeout = true
			err = KhpError{
				Errno:   KhpErrorReadTimeout,
				message: "Read timeout",
			}
			break
		}
	}

	if isTimeout {
		k.deadBroker(brokerID)
	} else {
		k.freeBroker(brokerID)
	}

	return offsets[0].result, offsets[1].result, err
}

// KafkaMetadata is a wrapper around metadata response
type KafkaMetadata struct {
	client   *KafkaClient
	Metadata *proto.MetadataResp
}

// GetMetadata returns metadata from kafka.
func (k *KafkaClient) GetMetadata() (meta *KafkaMetadata, err error) {
	brokerID, err := k.getBroker()
	if err != nil {
		return nil, err
	}

	defer k.Timings["GetMetadata"].Start().Stop()

	result := make(chan struct{})
	timeout := make(chan struct{})

	if k.GetMetadataTimeout > 0 {
		timer := time.AfterFunc(k.GetMetadataTimeout, func() { close(timeout) })
		defer timer.Stop()
	}

	var kafkaErr error
	meta = &KafkaMetadata{
		client: k,
	}

	go func() {
		meta.Metadata, kafkaErr = k.allBrokers[brokerID].Metadata()
		close(result)
	}()

	select {
	case <-result:
		k.freeBroker(brokerID)
		err = kafkaErr
	case <-timeout:
		k.deadBroker(brokerID)
		err = KhpError{
			Errno:   KhpErrorMetadataReadTimeout,
			message: "Read timeout",
		}
	}
	return
}

// FetchMetadata returns metadata from kafka but use internal cache.
func (k *KafkaClient) FetchMetadata() (*KafkaMetadata, error) {
	k.cache.RLock()
	defer k.cache.RUnlock()

	if k.MetadataCachePeriod > 0 && k.cache.lastUpdateMetadata > 0 {
		period := time.Now().UnixNano() - k.cache.lastUpdateMetadata

		if period < 0 {
			period = -period
		}

		if period < int64(k.MetadataCachePeriod) {
			return k.cache.lastMetadata, nil
		}
	}

	return k.GetMetadata()
}

// Topics returns list of known topics
func (m *KafkaMetadata) Topics() ([]string, error) {
	var topics []string

	for _, topic := range m.Metadata.Topics {
		if topic.Err != nil && topic.Err != proto.ErrLeaderNotAvailable {
			return nil, topic.Err
		}
		topics = append(topics, topic.Name)
	}

	return topics, nil
}

func (m *KafkaMetadata) inTopics(name string) (bool, error) {
	for _, topic := range m.Metadata.Topics {
		if topic.Err != nil {
			return false, topic.Err
		}

		if name == topic.Name {
			return true, nil
		}
	}
	return false, nil
}

type partitionType int

const (
	allPartitions partitionType = iota
	writablePartitions
	maxPartitionIndex
)

func (m *KafkaMetadata) getPartitions(topic string, pType partitionType) ([]int32, error) {
	var partitions []int32

	for _, t := range m.Metadata.Topics {
		if t.Err != nil {
			return nil, t.Err
		}

		if t.Name != topic {
			continue
		}

		for _, p := range t.Partitions {
			if pType == writablePartitions && p.Err == proto.ErrLeaderNotAvailable {
				continue
			}
			partitions = append(partitions, p.ID)
		}
	}

	return partitions, nil
}

// Partitions returns list of partitions.
func (m *KafkaMetadata) Partitions(topic string) ([]int32, error) {
	return m.getPartitions(topic, allPartitions)
}

// WritablePartitions returns list of partitions with a leader.
func (m *KafkaMetadata) WritablePartitions(topic string) ([]int32, error) {
	return m.getPartitions(topic, writablePartitions)
}

// Leader returns the ID of the node which is the leader for partition.
func (m *KafkaMetadata) Leader(topic string, partitionID int32) (int32, error) {
	for _, t := range m.Metadata.Topics {
		if t.Err != nil {
			return -1, t.Err
		}

		if t.Name != topic {
			continue
		}

		for _, p := range t.Partitions {
			if p.ID != partitionID {
				continue
			}
			return p.Leader, nil
		}
	}

	return -1, nil
}

// Replicas returns list of replicas for partition.
func (m *KafkaMetadata) Replicas(topic string, partitionID int32) ([]int32, error) {
	for _, t := range m.Metadata.Topics {
		if t.Err != nil {
			return nil, t.Err
		}

		if t.Name != topic {
			continue
		}

		for _, p := range t.Partitions {
			if p.ID != partitionID {
				continue
			}
			return p.Isrs, nil
		}
	}

	var isr []int32
	return isr, nil
}

// KafkaConsumer is a wrapper around kafka.Consumer.
type KafkaConsumer struct {
	client            *KafkaClient
	brokerID          int64
	consumer          kafka.Consumer
	opened            bool
	GetMessageTimeout time.Duration
}

// NewConsumer creates a new Consumer.
func (k *KafkaClient) NewConsumer(settings *Config, topic string, partitionID int32, offset int64) (*KafkaConsumer, error) {
	var err error

	brokerID, err := k.getBroker()
	if err != nil {
		return nil, err
	}

	conf := kafka.NewConsumerConf(topic, partitionID)

	conf.Logger = &kafkaLogger{
		subsys: "kafka/consumer",
	}

	conf.RequestTimeout = settings.Consumer.RequestTimeout.Duration
	conf.RetryLimit = settings.Consumer.RetryLimit
	conf.RetryWait = settings.Consumer.RetryWait.Duration
	conf.RetryErrLimit = settings.Consumer.RetryErrLimit
	conf.RetryErrWait = settings.Consumer.RetryErrWait.Duration
	conf.MinFetchSize = settings.Consumer.MinFetchSize
	conf.MaxFetchSize = settings.Consumer.MaxFetchSize
	conf.StartOffset = offset

	consumer, err := k.allBrokers[brokerID].Consumer(conf)
	if err != nil {
		k.freeBroker(brokerID)
		return nil, err
	}

	return &KafkaConsumer{
		client:            k,
		brokerID:          brokerID,
		consumer:          consumer,
		opened:            true,
		GetMessageTimeout: settings.Consumer.GetMessageTimeout.Duration,
	}, nil
}

// Close frees the connection and returns it to the free pool.
func (c *KafkaConsumer) Close() error {
	if c.opened {
		c.client.freeBroker(c.brokerID)
		c.opened = false
	}
	return nil
}

// Corrupt marks the connection as a broken.
func (c *KafkaConsumer) Corrupt() {
	if !c.opened {
		return
	}
	c.client.deadBroker(c.brokerID)
	c.opened = false
}

// Message returns message from kafka.
func (c *KafkaConsumer) Message() (msg *proto.Message, err error) {
	if !c.opened {
		err = KhpError{
			Errno:   KhpErrorConsumerClosed,
			message: "Read from closed consumer",
		}
		return
	}

	defer c.client.Timings["GetMessage"].Start().Stop()

	result := make(chan struct{})
	timeout := make(chan struct{})

	if c.GetMessageTimeout > 0 {
		timer := time.AfterFunc(c.GetMessageTimeout, func() { close(timeout) })
		defer timer.Stop()
	}

	var kafkaMsg *proto.Message
	var kafkaErr error

	go func() {
		kafkaMsg, kafkaErr = c.consumer.Consume()
		close(result)
	}()

	select {
	case <-result:
		msg, err = kafkaMsg, kafkaErr
	case <-timeout:
		c.Corrupt()
		err = KhpError{
			Errno:   KhpErrorReadTimeout,
			message: "Read timeout",
		}
	}
	return
}

// KafkaProducer is a wrapper around kafka.Producer.
type KafkaProducer struct {
	client             *KafkaClient
	brokerID           int64
	producer           kafka.Producer
	opened             bool
	SendMessageTimeout time.Duration
}

// NewProducer creates a new Producer.
func (k *KafkaClient) NewProducer(settings *Config) (*KafkaProducer, error) {
	brokerID, err := k.getBroker()
	if err != nil {
		return nil, err
	}

	conf := kafka.NewProducerConf()

	conf.Logger = &kafkaLogger{
		subsys: "kafka/producer",
	}

	conf.RequestTimeout = settings.Producer.RequestTimeout.Duration
	conf.RetryLimit = settings.Producer.RetryLimit
	conf.RetryWait = settings.Producer.RetryWait.Duration
	conf.RequiredAcks = proto.RequiredAcksAll

	return &KafkaProducer{
		client:             k,
		brokerID:           brokerID,
		producer:           k.allBrokers[brokerID].Producer(conf),
		opened:             true,
		SendMessageTimeout: settings.Producer.SendMessageTimeout.Duration,
	}, nil
}

// Close frees the connection and returns it to the free pool.
func (p *KafkaProducer) Close() error {
	if p.opened {
		p.client.freeBroker(p.brokerID)
		p.opened = false
	}
	return nil
}

// Corrupt marks the connection as a broken.
func (p *KafkaProducer) Corrupt() {
	if !p.opened {
		return
	}
	p.client.deadBroker(p.brokerID)
	p.opened = false
}

// SendMessage sends message in kafka.
func (p *KafkaProducer) SendMessage(topic string, partitionID int32, message []byte) (offset int64, err error) {
	if !p.opened {
		err = KhpError{
			Errno:   KhpErrorProducerClosed,
			message: "Write to closed producer",
		}
		return
	}

	defer p.client.Timings["SendMessage"].Start().Stop()

	result := make(chan struct{})
	timeout := make(chan struct{})

	if p.SendMessageTimeout > 0 {
		timer := time.AfterFunc(p.SendMessageTimeout, func() { close(timeout) })
		defer timer.Stop()
	}

	var kafkaOffset int64
	var kafkaErr error

	go func() {
		kafkaOffset, kafkaErr = p.producer.Produce(topic, partitionID, &proto.Message{
			Value: message,
		})
		close(result)
	}()

	select {
	case <-result:
		offset, err = kafkaOffset, kafkaErr
	case <-timeout:
		p.Corrupt()
		err = KhpError{
			Errno:   KhpErrorWriteTimeout,
			message: "Write timeout",
		}
	}
	return
}

// KafkaOffsetCoordinator is a wrapper around kafka.OffsetCoordinator.
type KafkaOffsetCoordinator struct {
	client              *KafkaClient
	brokerID            int64
	offsetCoordinator   kafka.OffsetCoordinator
	opened              bool
	CommitOffsetTimeout time.Duration
	FetchOffsetTimeout  time.Duration
}

// NewOffsetCoordinator creates a new KafkaOffsetCoordinator.
func (k *KafkaClient) NewOffsetCoordinator(settings *Config, consumerGroup string) (*KafkaOffsetCoordinator, error) {
	brokerID, err := k.getBroker()
	if err != nil {
		return nil, err
	}

	conf := kafka.NewOffsetCoordinatorConf(consumerGroup)

	conf.Logger = &kafkaLogger{
		subsys: "kafka/offset-coord",
	}

	conf.RetryErrLimit = settings.OffsetCoordinator.RetryErrLimit
	conf.RetryErrWait = settings.OffsetCoordinator.RetryErrWait.Duration

	coordinator, err := k.allBrokers[brokerID].OffsetCoordinator(conf)
	if err != nil {
		return nil, err
	}

	return &KafkaOffsetCoordinator{
		client:              k,
		brokerID:            brokerID,
		offsetCoordinator:   coordinator,
		opened:              true,
		CommitOffsetTimeout: settings.OffsetCoordinator.CommitOffsetTimeout.Duration,
		FetchOffsetTimeout:  settings.OffsetCoordinator.FetchOffsetTimeout.Duration,
	}, nil
}

// Close frees the connection and returns it to the free pool.
func (p *KafkaOffsetCoordinator) Close() error {
	if p.opened {
		p.client.freeBroker(p.brokerID)
		p.opened = false
	}
	return nil
}

// Corrupt marks the connection as a broken.
func (p *KafkaOffsetCoordinator) Corrupt() {
	if !p.opened {
		return
	}
	p.client.deadBroker(p.brokerID)
	p.opened = false
}

// CommitOffset commits consumer group offset of a given topic partition to kafka.
func (c *KafkaOffsetCoordinator) CommitOffset(topic string, partitionID int32, offset int64) (err error) {
	if !c.opened {
		err = KhpError{
			Errno:   KhpErrorOffsetCoordinatorClosed,
			message: "Write to closed offset coordinator",
		}
		return
	}

	defer c.client.Timings["CommitOffset"].Start().Stop()

	result := make(chan struct{})
	timeout := make(chan struct{})

	if c.CommitOffsetTimeout > 0 {
		timer := time.AfterFunc(c.CommitOffsetTimeout, func() { close(timeout) })
		defer timer.Stop()
	}

	var kafkaErr error

	go func() {
		kafkaErr = c.offsetCoordinator.Commit(topic, partitionID, offset)
		close(result)
	}()

	select {
	case <-result:
		err = kafkaErr
	case <-timeout:
		c.Corrupt()
		err = KhpError{
			Errno:   KhpErrorOffsetCommitTimeout,
			message: "Offset commit timeout",
		}
	}
	return
}

// FetchOffset returns consumer group offset of a given topic partition from kafka.
func (c *KafkaOffsetCoordinator) FetchOffset(topic string, partitionID int32) (offset int64, metadata string, err error) {
	if !c.opened {
		err = KhpError{
			Errno:   KhpErrorOffsetCoordinatorClosed,
			message: "Read from closed offset coordinator",
		}
		return
	}

	defer c.client.Timings["FetchOffset"].Start().Stop()

	result := make(chan struct{})
	timeout := make(chan struct{})

	if c.FetchOffsetTimeout > 0 {
		timer := time.AfterFunc(c.FetchOffsetTimeout, func() { close(timeout) })
		defer timer.Stop()
	}

	var kafkaOffset int64
	var kafkaMetadata string
	var kafkaErr error

	go func() {
		kafkaOffset, kafkaMetadata, kafkaErr = c.offsetCoordinator.Offset(topic, partitionID)
		close(result)
	}()

	select {
	case <-result:
		offset, metadata, err = kafkaOffset, kafkaMetadata, kafkaErr
	case <-timeout:
		c.Corrupt()
		err = KhpError{
			Errno:   KhpErrorOffsetFetchTimeout,
			message: "Offset fetch timeout",
		}
	}
	return
}
