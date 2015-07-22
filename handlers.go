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

// ResponseMessages is a templete for GET response.
type responseMessages struct {
	Query    kafkaParameters   `json:"query"`
	Messages []json.RawMessage `json:"messages"`
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
            <th class="text-right">Write to Kafka</p></th>
            <td>POST</td>
            <td><code>{schema}://{host}/v1/topics/{topic}/{partition}</code></td>
          </tr>
          <tr>
            <th class="text-right">Read from Kafka</th>
            <td>GET</td>
            <td><code>{schema}://{host}/v1/topics/{topic}/{partition}?offset={offset}&limit={limit}</code></td>
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

func (s *Server) sendHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	defer s.Stats.ResponsePostTime.Start().Stop()

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
		s.errorResponse(w, http.StatusBadRequest, "Message too large")
		return
	}

	var m json.RawMessage
	if err = json.Unmarshal(msg, &m); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Message must be JSON")
		return
	}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	parts, err := meta.Partitions(kafka.Topic)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
		return
	}

	if !inSlice(kafka.Partition, parts) {
		s.errorResponse(w, http.StatusBadRequest, "Partition not found")
		return
	}

	producer, err := s.Client.NewProducer(s.Cfg)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to make producer: %v", err)
		return
	}
	defer producer.Close()

	kafka.Offset, err = producer.SendMessage(kafka.Topic, kafka.Partition, msg)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "Unable to store your data: %v", err)
		return
	}

	s.MessageSize.Put(kafka.Topic, int32(len(msg)))
	s.successResponse(w, kafka)
}

func (s *Server) getHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	defer s.Stats.ResponseGetTime.Start().Stop()

	var (
		varsLength string
		varsOffset string
	)

	if varsLength = p.Get("limit"); varsLength == "" {
		varsLength = "1"
	}

	varsOffset = p.Get("offset")

	o := &responseMessages{
		Query: kafkaParameters{
			Topic:     p.Get("topic"),
			Partition: toInt32(p.Get("partition")),
			Offset:    -1,
		},
		Messages: []json.RawMessage{},
	}

	length := toInt32(varsLength)

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	parts, err := meta.Partitions(o.Query.Topic)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
		return
	}

	if !inSlice(o.Query.Partition, parts) {
		s.errorResponse(w, http.StatusBadRequest, "Partition not found")
		return
	}

	offsetFrom, err := meta.GetOffsetInfo(o.Query.Topic, o.Query.Partition, KafkaOffsetOldest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get offset: %v", err)
		return
	}

	if varsOffset == "" {
		// Set default value
		o.Query.Offset = offsetFrom
	} else {
		o.Query.Offset = toInt64(varsOffset)
	}

	offsetTo, err := meta.GetOffsetInfo(o.Query.Topic, o.Query.Partition, KafkaOffsetNewest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get offset: %v", err)
		return
	}

	offsetTo--

	if o.Query.Offset == 0 && offsetTo == 0 {
		// Topic is empty
		s.successResponse(w, o)
		return
	}

	if o.Query.Offset < offsetFrom || o.Query.Offset > offsetTo {
		s.errorOutOfRange(w, o.Query.Topic, o.Query.Partition, offsetFrom, offsetTo)
		return
	}

	cfg := s.Cfg
	offset := o.Query.Offset
	msgSize := s.MessageSize.Get(o.Query.Topic, s.Cfg.Consumer.DefaultFetchSize)
	incSize := false

ConsumeLoop:
	for {
		cfg.Consumer.MaxFetchSize = msgSize * length

		consumer, err := s.Client.NewConsumer(cfg, o.Query.Topic, o.Query.Partition, offset)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to make consumer: %v", err)
			return
		}
		defer consumer.Close()

		for {
			msg, err := consumer.Message()
			if err != nil {
				if err == KafkaErrNoData {
					incSize = true
					break
				}
				s.errorResponse(w, http.StatusInternalServerError, "Unable to get message: %v", err)
				consumer.Close()
				return
			}

			var m json.RawMessage

			if err := json.Unmarshal(msg.Value, &m); err != nil {
				s.errorResponse(w, http.StatusInternalServerError, "Bad JSON: %v", err)
				consumer.Close()
				return
			}
			o.Messages = append(o.Messages, m)

			offset = msg.Offset
			length--

			if msg.Offset >= offsetTo || length == 0 {
				consumer.Close()
				break ConsumeLoop
			}
		}

		if incSize {
			if msgSize >= s.Cfg.Consumer.MaxFetchSize {
				consumer.Close()
				break ConsumeLoop
			}

			msgSize += s.Cfg.Consumer.DefaultFetchSize

			if msgSize > s.Cfg.Consumer.MaxFetchSize {
				msgSize = s.Cfg.Consumer.MaxFetchSize
			}

			incSize = false
		}
	}

	if len(o.Messages) > 0 {
		s.MessageSize.Put(o.Query.Topic, msgSize)
	}

	s.successResponse(w, o)
}

func (s *Server) getTopicListHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	res := []responseTopicListInfo{}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	topics, err := meta.Topics()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get topics: %v", err)
		return
	}

	for _, topic := range topics {
		parts, err := meta.Partitions(topic)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
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
	res := &responsePartitionInfo{
		Topic:     p.Get("topic"),
		Partition: toInt32(p.Get("partition")),
	}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	res.Leader, err = meta.Leader(res.Topic, res.Partition)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get broker: %v", err)
		return
	}

	res.Replicas, err = meta.Replicas(res.Topic, res.Partition)
	if err != nil {
		if err != KafkaErrReplicaNotAvailable {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get replicas: %v", err)
			return
		}
		log.Printf("Error: Unable to get replicas: %v\n", err)
		res.Replicas = make([]int32, 0)
	}
	res.ReplicasNum = len(res.Replicas)

	res.OffsetNewest, err = meta.GetOffsetInfo(res.Topic, res.Partition, KafkaOffsetNewest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get newest offset: %v", err)
		return
	}

	res.OffsetOldest, err = meta.GetOffsetInfo(res.Topic, res.Partition, KafkaOffsetOldest)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get oldest offset: %v", err)
		return
	}

	wp, err := meta.WritablePartitions(res.Topic)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get writable partitions: %v", err)
		return
	}

	res.Writable = inSlice(res.Partition, wp)

	s.successResponse(w, res)
}

func (s *Server) getTopicInfoHandler(w *HTTPResponse, r *http.Request, p *url.Values) {
	res := []responsePartitionInfo{}

	meta, err := s.fetchMetadata()
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get metadata: %v", err)
		return
	}

	writable, err := meta.WritablePartitions(p.Get("topic"))
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get writable partitions: %v", err)
		return
	}

	parts, err := meta.Partitions(p.Get("topic"))
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "Unable to get partitions: %v", err)
		return
	}

	for partition := range parts {
		r := &responsePartitionInfo{
			Topic:     p.Get("topic"),
			Partition: int32(partition),
			Writable:  inSlice(int32(partition), writable),
		}

		r.Leader, err = meta.Leader(r.Topic, r.Partition)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get broker: %v", err)
			return
		}

		r.Replicas, err = meta.Replicas(r.Topic, r.Partition)
		if err != nil {
			if err != KafkaErrReplicaNotAvailable {
				s.errorResponse(w, http.StatusInternalServerError, "Unable to get replicas: %v", err)
				return
			}
			log.Printf("Error: Unable to get replicas: %v\n", err)
			r.Replicas = make([]int32, 0)
		}
		r.ReplicasNum = len(r.Replicas)

		r.OffsetNewest, err = meta.GetOffsetInfo(r.Topic, r.Partition, KafkaOffsetNewest)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get newest offset: %v", err)
			return
		}

		r.OffsetOldest, err = meta.GetOffsetInfo(r.Topic, r.Partition, KafkaOffsetOldest)
		if err != nil {
			s.errorResponse(w, http.StatusInternalServerError, "Unable to get oldest offset: %v", err)
			return
		}

		res = append(res, *r)
	}

	s.successResponse(w, res)
}