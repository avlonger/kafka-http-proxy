/*
* Copyright (C) 2015 Alexey Gladkov <gladkov.alexey@gmail.com>
*
* This file is covered by the GNU General Public License,
* which should be included with kafka-http-proxy as the file COPYING.
 */

package main

import (
	log "github.com/Sirupsen/logrus"

	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/url"
)

// KafkaParameters contains information about placement in Kafka. Used in GET/POST response.
type kafkaParameters struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
}

// ConsumerOffsetInfo contains information about consumer group offset of a topic partition. Used in GET/POST response.
type consumerOffsetInfo struct {
	Consumer  string `json:"consumer"`
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
	Metadata  string `json:"metadata"`
}

// ResponsePartitionInfo contains information about Kafka partition.
type responsePartitionInfo struct {
	Topic        string  `json:"topic"`
	Partition    int32   `json:"partition"`
	Leader       int32   `json:"leader"`
	OffsetOldest int64   `json:"offsetfrom"`
	OffsetNewest int64   `json:"offsetto"`
	Writable     bool    `json:"writable"`
	ReplicasNum  int     `json:"replicasnum"`
	Replicas     []int32 `json:"replicas"`
}

// ResponseTopicListInfo contains information about Kafka topic.
type responseTopicListInfo struct {
	Topic      string `json:"topic"`
	Partitions int    `json:"partitions"`
}

func httpStatusError(err error) int {
	if _, ok := err.(KhpError); ok {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func (s *Server) rootHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.rawResponse(w, http.StatusOK, []byte(`<!DOCTYPE html>
<html>
  <head>
    <meta charset="utf-8">
    <link href="http://yastatic.net/bootstrap/3.3.1/css/bootstrap.min.css" rel="stylesheet">
    <title>Endpoints | Kafka API v1</title>
  </head>
  <body>
    <div class="container"><h2>Kafka API v1</h2><br>
        <table class="table">
          <tr>
            <th class="text-right">Write to Kafka</th>
            <td>POST</td>
            <td><code>{schema}://{host}/v1/topics/{topic}/{partition}</code></td>
          </tr>
          <tr>
            <th class="text-right">Read from Kafka by absolute position</th>
            <td>GET</td>
            <td>
               <code>{schema}://{host}/v1/topics/{topic}/{partition}?offset={offset}&limit={limit}</code>
            </td>
          </tr>
          <tr>
            <th class="text-right">Read data relative to the beginning or end of the queue</th>
            <td>GET</td>
            <td>
               <p><code>{schema}://{host}/v1/topics/{topic}/{partition}?relative={position}&limit={limit}</code></p>
               The <b>{position}</b> can be positive or negative.
            </td>
          </tr>
          <tr>
            <th class="text-right">Obtain topic list</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/info/topics</code></td>
          </tr>
          <tr>
            <th class="text-right">Obtain information about all partitions in topic</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/info/topics/{topic}</code></td>
          </tr>
          <tr>
            <th class="text-right">Obtain information about partition</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/info/topics/{topic}/{partition}</code></td>
          </tr>
        </table>
    </div>
  </body>
</html>`))
}

func (s *Server) pingHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) notFoundHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	s.errorResponse(w, http.StatusNotFound, "404 page not found")
}

func (s *Server) notAllowedHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	s.errorResponse(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
}

func (s *Server) validRequest(w *HTTPResponse, p *url.Values) bool {
	topic := p.Get("topic")

	if topic == "" {
		s.errorResponse(w, http.StatusBadRequest, "Topic name required")
		return false
	}

	meta, err := s.Client.FetchMetadata()
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get metadata: %v", err)
		return false
	}

	found, err := meta.inTopics(topic)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get topic: %v", err)
		return false
	}

	if !found {
		s.errorResponse(w, http.StatusBadRequest, "Topic unknown")
		return false
	}

	if p.Get("partition") == "" {
		return true
	}

	partition := toInt32(p.Get("partition"))

	parts, err := meta.Partitions(topic)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get partitions: %v", err)
		return false
	}

	if !inSlice(partition, parts) {
		s.errorResponse(w, http.StatusBadRequest, "Unknown partition for the specified topic")
		return false
	}

	return true
}

func (s *Server) sendHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	defer s.Stats.HTTPResponseTime["POST"].Start().Stop()

	kafka := &kafkaParameters{
		Topic:     p.Get("topic"),
		Partition: toInt32(p.Get("partition")),
		Offset:    -1,
	}

	msg, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Unable to read body: %s", err)
		return
	}

	if int32(len(msg)) > s.Cfg.Consumer.MaxFetchSize {
		s.errorResponse(w, http.StatusBadRequest, "Message too large: Body size should be less than %d, but it is %d", s.Cfg.Consumer.MaxFetchSize, int32(len(msg)))
		return
	}

	var m json.RawMessage
	if err = json.Unmarshal(msg, &m); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Message must be JSON")
		return
	}

	if !s.validRequest(w, p) {
		return
	}

	producer, err := s.Client.NewProducer(s.Cfg)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to make producer: %v", err)
		return
	}
	defer producer.Close()

	kafka.Offset, err = producer.SendMessage(kafka.Topic, kafka.Partition, msg)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to store your data: %v", err)
		return
	}

	s.MessageSize.Put(kafka.Topic, int32(len(msg)))
	s.successResponse(w, kafka)
}

func (s *Server) getHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	defer s.Stats.HTTPResponseTime["GET"].Start().Stop()

	var (
		varsLength   string
		varsOffset   string
		varsRelative string
	)

	if varsLength = p.Get("limit"); varsLength == "" {
		varsLength = "1"
	}

	varsOffset = p.Get("offset")
	varsRelative = p.Get("relative")

	query := kafkaParameters{
		Topic:     p.Get("topic"),
		Partition: toInt32(p.Get("partition")),
		Offset:    -1,
	}

	length := toInt32(varsLength)
	if length <= 0 {
		length = 1
	}

	if !s.validRequest(w, p) {
		return
	}

	offsetFrom, offsetTo, err := s.Client.GetOffsets(query.Topic, query.Partition)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get offset: %v", err)
		return
	}

	if varsRelative != "" {
		relative := toInt64(varsRelative)

		if relative >= 0 {
			query.Offset = offsetFrom + relative
		} else {
			query.Offset = offsetTo + relative
		}
	} else if varsOffset != "" {
		query.Offset = toInt64(varsOffset)
	} else {
		// Set default value
		query.Offset = offsetFrom
	}

	if query.Offset < offsetFrom || query.Offset >= offsetTo {
		s.errorOutOfRange(w, query.Topic, query.Partition, offsetFrom, offsetTo)
		return
	}

	queryStr, err := json.Marshal(query)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to marshal json: %v", err)
		return
	}

	cfg := *s.Cfg
	offset := query.Offset
	size := s.MessageSize.Get(query.Topic, s.Cfg.Consumer.DefaultFetchSize)
	maxSize := 0

	notEnoughSize := false
	successSent := false

ConsumeLoop:
	for {
		cfg.Consumer.MaxFetchSize = size * length

		if cfg.Consumer.MaxFetchSize > s.Cfg.Consumer.MaxFetchSize {
			cfg.Consumer.MaxFetchSize = s.Cfg.Consumer.MaxFetchSize
		}

		consumer, err := s.Client.NewConsumer(&cfg, query.Topic, query.Partition, offset)
		if err != nil {
			if !successSent {
				s.errorResponse(w, httpStatusError(err), "Unable to make consumer: %v", err)
			}
			return
		}
		defer consumer.Close()

		for {
			if !s.connIsAlive(w) {
				consumer.Close()
				return
			}

			msg, err := consumer.Message()
			if err != nil {
				if err == KafkaErrNoData {
					notEnoughSize = true
					break
				}
				if !successSent {
					s.errorResponse(w, httpStatusError(err), "Unable to get message: %v", err)
				}
				consumer.Close()
				return
			}

			if !successSent {
				successSent = true

				s.beginResponse(w, http.StatusOK)
				w.Write([]byte(`{`))
				w.Write([]byte(`"query":`))
				w.Write(queryStr)
				w.Write([]byte(`,"messages":[`))
			} else {
				w.Write([]byte(`,`))
			}

			w.Write(msg.Value)

			offset = msg.Offset + 1
			length--

			if len(msg.Value) > maxSize {
				maxSize = len(msg.Value)
			}

			if offset >= offsetTo || length == 0 {
				consumer.Close()
				break ConsumeLoop
			}
		}
		consumer.Close()

		if notEnoughSize {
			if size >= s.Cfg.Consumer.MaxFetchSize {
				break ConsumeLoop
			}

			size += s.Cfg.Consumer.DefaultFetchSize
			notEnoughSize = false
		}
	}

	if !successSent {
		s.beginResponse(w, http.StatusOK)
		w.Write([]byte(`{`))
		w.Write([]byte(`"query":`))
		w.Write(queryStr)
		w.Write([]byte(`,"messages":[`))
	}

	w.Write([]byte(`]}`))
	s.endResponseSuccess(w)

	if maxSize > 0 {
		s.MessageSize.Put(query.Topic, int32(maxSize))
	}
}

func (s *Server) getOffsetHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	defer s.Stats.HTTPResponseTime["FetchOffset"].Start().Stop()

	kafka := &consumerOffsetInfo{
		Consumer:  p.Get("consumer"),
		Topic:     p.Get("topic"),
		Partition: toInt32(p.Get("partition")),
		Offset:    -1,
	}

	if !s.validRequest(w, p) {
		return
	}

	if kafka.Consumer == "" {
		s.errorResponse(w, http.StatusBadRequest, "Consumer name must be provided")
		return
	}

	offsetCoordinator, err := s.Client.NewOffsetCoordinator(s.Cfg, kafka.Consumer)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to make offset coordinator: %v", err)
		return
	}
	defer offsetCoordinator.Close()

	kafka.Offset, kafka.Metadata, err = offsetCoordinator.FetchOffset(kafka.Topic, kafka.Partition)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to fetch offset: %v", err)
		return
	}

	s.successResponse(w, kafka)
}

func (s *Server) commitOffsetHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	defer s.Stats.HTTPResponseTime["CommitOffset"].Start().Stop()

	msg, err := ioutil.ReadAll(r.Body)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Unable to read body: %s", err)
		return
	}

	kafka := &consumerOffsetInfo{
		Offset:    -1,
	}

	if err = json.Unmarshal(msg, &kafka); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Request body must be JSON")
		return
	}

	if kafka.Offset < 0 {
		s.errorResponse(w, http.StatusBadRequest, "Offset must be provided not less than 0")
		return
	}

	kafka.Consumer = p.Get("consumer")
	kafka.Topic = p.Get("topic")
	kafka.Partition = toInt32(p.Get("partition"))

	if !s.validRequest(w, p) {
		return
	}

	if kafka.Consumer == "" {
		s.errorResponse(w, http.StatusBadRequest, "Consumer name must be provided")
		return
	}

	offsetCoordinator, err := s.Client.NewOffsetCoordinator(s.Cfg, kafka.Consumer)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to make offset coordinator: %v", err)
		return
	}
	defer offsetCoordinator.Close()

	err = offsetCoordinator.CommitOffset(kafka.Topic, kafka.Partition, kafka.Offset)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to commit offset: %v", err)
		return
	}
	s.successResponse(w, kafka)
}

func (s *Server) getTopicListHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	defer s.Stats.HTTPResponseTime["GetTopicList"].Start().Stop()

	res := []responseTopicListInfo{}

	meta, err := s.Client.FetchMetadata()
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get metadata: %v", err)
		return
	}

	topics, err := meta.Topics()
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get topics: %v", err)
		return
	}

	for _, topic := range topics {
		parts, err := meta.Partitions(topic)
		if err != nil {
			s.errorResponse(w, httpStatusError(err), "Unable to get partitions: %v", err)
			return
		}
		info := &responseTopicListInfo{
			Topic:      topic,
			Partitions: len(parts),
		}
		res = append(res, *info)
	}

	s.successResponse(w, res)
}

func (s *Server) getPartitionInfoHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	if !s.validRequest(w, p) {
		return
	}

	defer s.Stats.HTTPResponseTime["GetPartitionInfo"].Start().Stop()

	res := &responsePartitionInfo{
		Topic:     p.Get("topic"),
		Partition: toInt32(p.Get("partition")),
	}

	meta, err := s.Client.FetchMetadata()
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get metadata: %v", err)
		return
	}

	res.Leader, err = meta.Leader(res.Topic, res.Partition)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get broker: %v", err)
		return
	}

	res.Replicas, err = meta.Replicas(res.Topic, res.Partition)
	if err != nil {
		if err != KafkaErrReplicaNotAvailable {
			s.errorResponse(w, httpStatusError(err), "Unable to get replicas: %v", err)
			return
		}
		log.Printf("Error: Unable to get replicas: %v\n", err)
		res.Replicas = make([]int32, 0)
	}
	res.ReplicasNum = len(res.Replicas)

	res.OffsetOldest, res.OffsetNewest, err = s.Client.GetOffsets(res.Topic, res.Partition)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get offset: %v", err)
		return
	}

	wp, err := meta.WritablePartitions(res.Topic)
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get writable partitions: %v", err)
		return
	}

	res.Writable = inSlice(res.Partition, wp)

	s.successResponse(w, res)
}

func (s *Server) getTopicInfoHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	if !s.validRequest(w, p) {
		return
	}

	defer s.Stats.HTTPResponseTime["GetTopicInfo"].Start().Stop()

	res := []responsePartitionInfo{}

	meta, err := s.Client.FetchMetadata()
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get metadata: %v", err)
		return
	}

	writable, err := meta.WritablePartitions(p.Get("topic"))
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get writable partitions: %v", err)
		return
	}

	parts, err := meta.Partitions(p.Get("topic"))
	if err != nil {
		s.errorResponse(w, httpStatusError(err), "Unable to get partitions: %v", err)
		return
	}

	for partition := range parts {
		if !s.connIsAlive(w) {
			return
		}

		r := &responsePartitionInfo{
			Topic:     p.Get("topic"),
			Partition: int32(partition),
			Writable:  inSlice(int32(partition), writable),
		}

		r.Leader, err = meta.Leader(r.Topic, r.Partition)
		if err != nil {
			s.errorResponse(w, httpStatusError(err), "Unable to get broker: %v", err)
			return
		}

		r.Replicas, err = meta.Replicas(r.Topic, r.Partition)
		if err != nil {
			if err != KafkaErrReplicaNotAvailable {
				s.errorResponse(w, httpStatusError(err), "Unable to get replicas: %v", err)
				return
			}
			log.Printf("Error: Unable to get replicas: %v\n", err)
			r.Replicas = make([]int32, 0)
		}
		r.ReplicasNum = len(r.Replicas)

		r.OffsetOldest, r.OffsetNewest, err = s.Client.GetOffsets(r.Topic, r.Partition)
		if err != nil {
			s.errorResponse(w, httpStatusError(err), "Unable to get offset: %v", err)
			return
		}

		res = append(res, *r)
	}

	s.successResponse(w, res)
}
