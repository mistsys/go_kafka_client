/* Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License. */

package main

import (
	"flag"
	"fmt"
	syslog "github.com/mcuadros/go-syslog"
	kafka "github.com/stealthly/go_kafka_client"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"math"
	"github.com/jeromer/syslogparser"
	"github.com/Shopify/sarama"
	sp "github.com/stealthly/go_kafka_client/syslog/syslog_proto"
	"encoding/json"
	"github.com/golang/protobuf/proto"
	"time"
)

type tags map[string]string

func (i tags) String() string {
	return "tags"
}

func (i tags) Set(value string) error {
	defer func() {
		r := recover()
		if r != nil {
			fmt.Println("Tags are expected in k=v format.")
			os.Exit(1)
		}
	}()

	kv := strings.Split(value, "=")
	i[kv[0]] = kv[1]
	return nil
}

var logLevel = flag.String("log.level", "info", "Log level for built-in logger.")
var producerConfig = flag.String("producer.config", "", "Path to producer configuration file.")
var numProducers = flag.Int("num.producers", 1, "Number of producers.")
var queueSize = flag.Int("queue.size", 10000, "Number of messages that are buffered between the consumer and producer.")
var topic = flag.String("topic", "", "Topic to produce messages into.")
var format = flag.String("format", "rfc5424", "Message format. Either RFC5424 or RFC3164.")
var tcpPort = flag.String("tcp.port", "5140", "TCP port to listen for incoming messages.")
var tcpHost = flag.String("tcp.host", "0.0.0.0", "TCP host to listen for incoming messages.")
var udpPort = flag.String("udp.port", "5141", "UDP port to listen for incoming messages.")
var udpHost = flag.String("udp.host", "0.0.0.0", "UDP host to listen for incoming messages.")
var maxProcs = flag.Int("max.procs", runtime.NumCPU(), "Maximum number of CPUs that can be executing simultaneously.")

//additional params
var source = flag.String("source", "", "")
var tag tags
var logtypeid = flag.Int64("log.type.id", math.MinInt64, "")

func parseAndValidateArgs() *kafka.SyslogProducerConfig {
	tag = make(map[string]string)
	flag.Var(tag, "tag", "")
	flag.Parse()

	setLogLevel()
	runtime.GOMAXPROCS(*maxProcs)
	rfc5424 := "rfc5424"
	rfc3164 := "rfc3164"

	if *topic == "" {
		fmt.Println("Topic is required.")
		os.Exit(1)
	}
	if *producerConfig == "" {
		fmt.Println("Producer config is required.")
		os.Exit(1)
	}
	if *queueSize < 0 {
		fmt.Println("Queue size should be equal or greater than 0")
		os.Exit(1)
	}

	config := kafka.NewSyslogProducerConfig()
	conf, err := kafka.ProducerConfigFromFile(*producerConfig)
	if err != nil {
		panic(err)
	}
	if err = conf.Validate(); err != nil {
		panic(err)
	}
	config.ProducerConfig = conf
	config.NumProducers = *numProducers
	config.ChannelSize = *queueSize
	config.Topic = *topic
	if strings.ToLower(*format) == rfc5424 {
		config.Format = syslog.RFC5424
	} else if strings.ToLower(*format) == rfc3164 {
		config.Format = syslog.RFC3164
	} else {
		fmt.Println("Message format can be RFC5424 or RFC3164 (any case).")
		os.Exit(1)
	}
	config.TCPAddr = fmt.Sprintf("%s:%s", *tcpHost, *tcpPort)
	config.UDPAddr = fmt.Sprintf("%s:%s", *udpHost, *udpPort)

	if !(*source == "" && len(tag) == 0 && *logtypeid == math.MinInt64) {
		config.Transformer = protobufTransformer
	}

	return config
}

func setLogLevel() {
	var level kafka.LogLevel
	switch strings.ToLower(*logLevel) {
	case "trace":
		level = kafka.TraceLevel
	case "debug":
		level = kafka.DebugLevel
	case "info":
		level = kafka.DebugLevel
	case "warn":
		level = kafka.DebugLevel
	case "error":
		level = kafka.DebugLevel
	case "critical":
		level = kafka.DebugLevel
	default:
	{
		fmt.Printf("Invalid log level: %s\n", *logLevel)
		os.Exit(1)
	}
	}
	kafka.Logger = kafka.NewDefaultLogger(level)
}

func main() {
	config := parseAndValidateArgs()
	producer := kafka.NewSyslogProducer(config)
	go producer.Start()

	ctrlc := make(chan os.Signal, 1)
	signal.Notify(ctrlc, os.Interrupt)
	<-ctrlc
	producer.Stop()
}

func protobufTransformer(msg syslogparser.LogParts, topic string) *sarama.MessageToSend {
	line := &sp.LogLine{}

	b, err := json.Marshal(msg)
	if err != nil {
		kafka.Errorf("protobuf-transformer", "Failed to marshal %s as JSON", msg)
	}
	line.Line = proto.String(string(b))
	line.Source = proto.String(*source)
	for k, v := range tag {
		line.Tag = append(line.Tag, &sp.LogLine_Tag{Key: proto.String(k), Value: proto.String(v)})
	}
	if *logtypeid != math.MinInt64 {
		line.Logtypeid = logtypeid
	}
	line.Timings = append(line.Timings, time.Now().Unix())

	protobuf, err := proto.Marshal(line)
	if err != nil {
		kafka.Errorf("protobuf-transformer", "Failed to marshal %s as Protocol Buffer", msg)
	}

	return &sarama.MessageToSend{Topic: topic, Key: sarama.StringEncoder(*source), Value: sarama.ByteEncoder(protobuf)}
}
