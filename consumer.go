/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 * 
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package go_kafka_client

import (
	"time"
	"github.com/samuel/go-zookeeper/zk"
	"os"
	"os/signal"
	"sync"
	"fmt"
	"github.com/Shopify/sarama"
	"reflect"
)

var InvalidOffset int64 = -1

var SmallestOffset = "smallest"

type Consumer struct {
	config        *ConsumerConfig
	zookeeper      []string
	fetcher         *consumerFetcherManager
	messages       chan *Message
	unsubscribe    chan bool
	closeFinished  chan bool
	zkConn          *zk.Conn
	rebalanceLock  sync.Mutex
	isShuttingdown bool
	topicChannels map[string][]<-chan []*Message
	topicThreadIdsAndSharedChannels map[TopicAndThreadId]*SharedBlockChannel
	TopicRegistry map[string]map[int]*PartitionTopicInfo
	checkPointedZkOffsets map[*TopicAndPartition]int64
	closeChannels  []chan bool
}

type Message struct {
	Key       []byte
	Value     []byte
	Topic     string
	Partition int32
	Offset    int64
}

func NewConsumer(config *ConsumerConfig) *Consumer {
	Infof(config.ConsumerId, "Starting new consumer with configuration: %s", config)
	c := &Consumer{
		config : config,
		messages : make(chan *Message),
		unsubscribe : make(chan bool, 1),
		closeFinished : make(chan bool),
		topicChannels : make(map[string][]<-chan []*Message),
		topicThreadIdsAndSharedChannels : make(map[TopicAndThreadId]*SharedBlockChannel),
		TopicRegistry: make(map[string]map[int]*PartitionTopicInfo),
		checkPointedZkOffsets: make(map[*TopicAndPartition]int64),
		closeChannels: make([]chan bool, 0),
	}

	c.addShutdownHook()

	c.connectToZookeeper()
	c.fetcher = newConsumerFetcherManager(config, c.zkConn, c.messages)

	return c
}

func (c *Consumer) String() string {
	return c.config.ConsumerId
}

func (c *Consumer) CreateMessageStreams(topicCountMap map[string]int) map[string][]<-chan []*Message {
	staticTopicCount := &StaticTopicsToNumStreams {
		ConsumerId : c.config.ConsumerId,
		TopicsToNumStreamsMap : topicCountMap,
	}

	var channelsAndStreams []*ChannelAndStream = nil
	for _, threadIdSet := range staticTopicCount.GetConsumerThreadIdsPerTopic() {
		channelsAndStreamsForThread := make([]*ChannelAndStream, len(threadIdSet))
		for i := 0; i < len(channelsAndStreamsForThread); i++ {
			closeChannel := make(chan bool, 1)
			c.closeChannels = append(c.closeChannels, closeChannel)
			channelsAndStreamsForThread[i] = NewChannelAndStream(c.config, closeChannel)
		}
		channelsAndStreams = append(channelsAndStreams, channelsAndStreamsForThread...)
	}

	c.RegisterInZK(staticTopicCount)
	c.ReinitializeConsumer(staticTopicCount, channelsAndStreams)

	return c.topicChannels
}

func (c *Consumer) CreateMessageStreamsByFilterN(topicFilter TopicFilter, numStreams int) []<-chan []*Message {
	var channelsAndStreams []*ChannelAndStream = nil
	for i := 0; i < numStreams; i++ {
		closeChannel := make(chan bool, 1)
		c.closeChannels = append(c.closeChannels, closeChannel)
		channelsAndStreams = append(channelsAndStreams, NewChannelAndStream(c.config, closeChannel))
	}
	allTopics, err := GetTopics(c.zkConn)
	if err != nil {
		panic(err)
	}
	filteredTopics := make([]string, 0)
	for _, topic := range allTopics {
		if topicFilter.IsTopicAllowed(topic, c.config.ExcludeInternalTopics) {
			filteredTopics = append(filteredTopics, topic)
		}
	}
	topicCount := &WildcardTopicsToNumStreams{
		ZkConnection : c.zkConn,
		ConsumerId : c.config.ConsumerId,
		TopicFilter : topicFilter,
		NumStreams : numStreams,
		ExcludeInternalTopics : c.config.ExcludeInternalTopics,
	}

	c.RegisterInZK(topicCount)
	c.ReinitializeConsumer(topicCount, channelsAndStreams)

	//TODO subscriptions?

	messages := make([]<-chan []*Message, 0)
	for _, channelAndStream := range channelsAndStreams {
		messages = append(messages, channelAndStream.Messages)
	}

	return messages
}

func (c *Consumer) CreateMessageStreamsByFilter(topicFilter TopicFilter) []<-chan []*Message {
	return c.CreateMessageStreamsByFilterN(topicFilter, c.config.NumConsumerFetchers)
}

func (c *Consumer) RegisterInZK(topicCount TopicsToNumStreams) {
	RegisterConsumer(c.zkConn, c.config.Groupid, c.config.ConsumerId, &ConsumerInfo{
			Version : int16(1),
			Subscription : topicCount.GetTopicsToNumStreamsMap(),
			Pattern : topicCount.Pattern(),
			Timestamp : time.Now().Unix(),
		})
}

func (c *Consumer) ReinitializeConsumer(topicCount TopicsToNumStreams, channelsAndStreams []*ChannelAndStream) {
	consumerThreadIdsPerTopic := topicCount.GetConsumerThreadIdsPerTopic()

	allChannelsAndStreams := make([]*ChannelAndStream, 0)
	switch topicCount.(type) {
		case *StaticTopicsToNumStreams: {
			allChannelsAndStreams = channelsAndStreams
		}
		case *WildcardTopicsToNumStreams: {
			for _, _ = range consumerThreadIdsPerTopic {
				for _, channelAndStream := range channelsAndStreams {
					allChannelsAndStreams = append(allChannelsAndStreams, channelAndStream)
				}
			}
		}
	}
	topicThreadIds := make([]TopicAndThreadId, 0)
	for topic, threadIds := range consumerThreadIdsPerTopic {
		for _, threadId := range threadIds {
			topicThreadIds = append(topicThreadIds, TopicAndThreadId{topic, threadId})
		}
	}

	if len(topicThreadIds) != len(allChannelsAndStreams) {
		panic("Mismatch between thread ID count and channel count")
	}
	threadStreamPairs := make(map[TopicAndThreadId]*ChannelAndStream)
	for i := 0; i < len(topicThreadIds); i++ {
		threadStreamPairs[topicThreadIds[i]] = allChannelsAndStreams[i]
	}

	for topicThread, channelStream := range threadStreamPairs {
		c.topicThreadIdsAndSharedChannels[topicThread] = channelStream.Blocks
	}

	topicToStreams := make(map[string][]<-chan []*Message)
	for topicThread, channelStream := range threadStreamPairs {
		topic := topicThread.Topic
		if topicToStreams[topic] == nil {
			topicToStreams[topic] = make([]<-chan []*Message, 0)
		}
		topicToStreams[topic] = append(topicToStreams[topic], channelStream.Messages)
	}
	c.topicChannels = topicToStreams

	c.subscribeForChanges(c.config.Groupid)
	//TODO more subscriptions

	c.rebalance()
}

func (c *Consumer) SwitchTopic(topicCountMap map[string]int, pattern string) {
	Infof(c, "Switching to %s with pattern '%s'", topicCountMap, pattern)
	//TODO: whitelist/blacklist pattern handling
	staticTopicCount := &TopicSwitch {
		ConsumerId : c.config.ConsumerId,
		TopicsToNumStreamsMap : topicCountMap,
		DesiredPattern: pattern,
	}

	RegisterConsumer(c.zkConn, c.config.Groupid, c.config.ConsumerId, &ConsumerInfo{
		Version : int16(1),
		Subscription : staticTopicCount.GetTopicsToNumStreamsMap(),
		Pattern : fmt.Sprintf("%s%s", SwitchToPatternPrefix, staticTopicCount.Pattern()),
		Timestamp : time.Now().Unix(),
	})
	err := NotifyConsumerGroup(c.zkConn, c.config.Groupid, c.config.ConsumerId)
	if err != nil {
		panic(err)
	}
}

func (c *Consumer) Close() <-chan bool {
	Info(c, "Closing consumer")
	c.isShuttingdown = true
	go func() {
		Info(c, "Closing channels")
		for _, ch := range c.closeChannels {
			ch <- true
		}
		Info(c, "Closing fetcher")
		<-c.fetcher.Close()
		Info(c, "Unsubscribing")
		c.unsubscribe <- true
		Info(c, "Finished")
		c.closeFinished <- true
	}()
	return c.closeFinished
}

func (c *Consumer) updateFetcher() {
	allPartitionInfos := make([]*PartitionTopicInfo, 0)
	for _, partitionAndInfo := range c.TopicRegistry {
		for _, partitionInfo := range partitionAndInfo {
			allPartitionInfos = append(allPartitionInfos, partitionInfo)
		}
	}

	c.fetcher.startConnections(allPartitionInfos)
}

func (c *Consumer) Ack(offset int64, topic string, partition int32) error {
	Infof(c, "Acking offset %d for topic %s and partition %d", offset, topic, partition)
	return nil
}

func (c *Consumer) addShutdownHook() {
	s := make(chan os.Signal, 1)
	signal.Notify(s, os.Interrupt)
	go func() {
		<-s
		c.Close()
	}()
}

func (c *Consumer) connectToZookeeper() {
	Infof(c, "Connecting to ZK at %s\n", c.config.ZookeeperConnect)
	if conn, _, err := zk.Connect(c.config.ZookeeperConnect, c.config.ZookeeperTimeout); err != nil {
		panic(err)
	} else {
		c.zkConn = conn
	}
}

func (c *Consumer) subscribeForChanges(group string) {
	dirs := NewZKGroupDirs(group)
	Infof(c, "Subscribing for changes for %s", dirs.ConsumerRegistryDir)
	err := CreateOrUpdatePathParentMayNotExist(c.zkConn, dirs.ConsumerChangesDir, make([]byte, 0))
	if err != nil {
		panic(err)
	}

	consumersWatcher, err := GetConsumersInGroupWatcher(c.zkConn, group)
	if err != nil {
		panic(err)
	}
	consumerGroupChangesWatcher, err := GetConsumerGroupChangesWatcher(c.zkConn, group)
	if err != nil {
		panic(err)
	}
	topicsWatcher, err := GetTopicsWatcher(c.zkConn)
	if err != nil {
		panic(err)
	}
	brokersWatcher, err := GetAllBrokersInClusterWatcher(c.zkConn)
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			select {
			case e := <-topicsWatcher: {
				Trace(c, e)
				if e.State == zk.StateDisconnected {
					Debug(c, "Topic registry watcher session ended, reconnecting...")
					watcher, err := GetTopicsWatcher(c.zkConn)
					if err != nil {
						panic(err)
					}
					topicsWatcher = watcher
				} else {
					InLock(&c.rebalanceLock, func() { triggerRebalanceIfNeeded(e, c) })
				}
			}
			case e := <-consumersWatcher: {
				Trace(c, e)
				if e.State == zk.StateDisconnected {
					Debug(c, "Consumer registry watcher session ended, reconnecting...")
					watcher, err := GetConsumersInGroupWatcher(c.zkConn, group)
					if err != nil {
						panic(err)
					}
					consumersWatcher = watcher
				} else {
					InLock(&c.rebalanceLock, func() { triggerRebalanceIfNeeded(e, c) })
				}
			}
			case e := <-brokersWatcher: {
				Trace(c, e)
				if e.State == zk.StateDisconnected {
					Debug(c, "Broker registry watcher session ended, reconnecting...")
					watcher, err := GetAllBrokersInClusterWatcher(c.zkConn)
					if err != nil {
						panic(err)
					}
					brokersWatcher = watcher
				} else {
					InLock(&c.rebalanceLock, func() { triggerRebalanceIfNeeded(e, c) })
				}
			}
			case e := <-consumerGroupChangesWatcher: {
				Trace(c, e)
				if e.State == zk.StateDisconnected {
					Debug(c, "Consumer changes watcher session ended, reconnecting...")
					watcher, err := GetConsumerGroupChangesWatcher(c.zkConn, group)
					if err != nil {
						panic(err)
					}
					consumerGroupChangesWatcher = watcher
				} else {
					InLock(&c.rebalanceLock, func() { triggerRebalanceIfNeeded(e, c) })
				}
			}
			case <-c.unsubscribe: {
				Debug(c, "Unsubscribing from changes")
				c.releasePartitionOwnership(c.TopicRegistry)
				err := DeregisterConsumer(c.zkConn, c.config.Groupid, c.config.ConsumerId)
				if err != nil {
					panic(err)
				}
				break
			}
			}
		}
	}()
}

func triggerRebalanceIfNeeded(e zk.Event, c *Consumer) {
	emptyEvent := zk.Event{}
	if e != emptyEvent {
		c.rebalance()
	} else {
		time.Sleep(2 * time.Second)
	}
}

func (c *Consumer) rebalance() {
	partitionAssignor := NewPartitionAssignor(c.config.PartitionAssignmentStrategy)
	if (!c.isShuttingdown) {
		Infof(c, "rebalance triggered for %s\n", c.config.ConsumerId)
		var success = false
		for i := 0; i < int(c.config.RebalanceMaxRetries); i++ {
			if (tryRebalance(c, partitionAssignor)) {
				success = true
				break
			} else {
				time.Sleep(c.config.RebalanceBackoffMs)
			}
		}

		if (!success && !c.isShuttingdown) {
			panic(fmt.Sprintf("Failed to rebalance after %d retries", c.config.RebalanceMaxRetries))
		}
	} else {
		Infof(c, "Rebalance was triggered during consumer '%s' shutdown sequence. Ignoring...\n", c.config.ConsumerId)
	}
}

func tryRebalance(c *Consumer, partitionAssignor AssignStrategy) bool {
	//Don't hurry to delete it, we need it for closing the fetchers
	topicPerThreadIdsMap, err := NewTopicsToNumStreams(c.config.Groupid, c.config.ConsumerId, c.zkConn, c.config.ExcludeInternalTopics)
	if (err != nil) {
		Errorf(c, "Failed to get topic count map: %s", err)
		return false
	}
	Infof(c, "%v\n", topicPerThreadIdsMap)

	brokers, err := GetAllBrokersInCluster(c.zkConn)
	if (err != nil) {
		Errorf(c, "Failed to get broker list: %s", err)
		return false
	}
	Infof(c, "%v\n", brokers)

	//TODO: close fetchers
	Debug(c, c.TopicRegistry)
	c.releasePartitionOwnership(c.TopicRegistry)

	assignmentContext, err := NewAssignmentContext(c.config.Groupid, c.config.ConsumerId, c.config.ExcludeInternalTopics, c.zkConn)
	if err != nil {
		Errorf(c, "Failed to initialize assignment context: %s", err)
		return false
	}
	if assignmentContext.State.IsGroupTopicSwitchInProgress {
		Info(c, "Consumer group is in process of switching to new topics")
		if !assignmentContext.InTopicSwitch {
			c.SwitchTopic(assignmentContext.State.DesiredTopicCountMap, assignmentContext.State.DesiredPattern)
			return true
		}

		if !assignmentContext.State.IsGroupTopicSwitchInSync {
			zkGroupInSync, err := IsConsumerGroupInSync(c.zkConn, c.config.Groupid)
			if err != nil {
				Error(c, "Failed to get group sync state")
				return false
			}
			if !zkGroupInSync {
				Infof(c, "Group is not sync yet. Waiting...")
				return true
			}
		} else {
			err = CreateConsumerGroupSync(c.zkConn, c.config.Groupid)
			if err != nil {
				Error(c, "Failed to initialize assignment context")
				return false
			}
		}

		RegisterConsumer(c.zkConn, assignmentContext.Group, assignmentContext.ConsumerId, &ConsumerInfo{
			Version : int16(1),
			Subscription : assignmentContext.State.DesiredTopicCountMap,
			Pattern : assignmentContext.State.DesiredPattern,
			Timestamp : time.Now().Unix(),
		})
		err = NotifyConsumerGroup(c.zkConn, c.config.Groupid, c.config.ConsumerId)
		if (err != nil) {
			Errorf(c, "Failed to notify consumer group: %s", err)
			return false
		}
	} else {
		err = DeleteConsumerGroupSync(c.zkConn, c.config.Groupid)
		if err != nil {
			Errorf(c, "Failed to delete consumer group sync: %s", err)
		}
		err = PurgeObsoleteConsumerGroupNotifications(c.zkConn, c.config.Groupid)
		if err != nil {
			Errorf(c, "Failed to delete obsolete notifications for consumer group: %s", err)
		}
	}

	partitionOwnershipDecision := partitionAssignor(assignmentContext)
	topicPartitions := make([]*TopicAndPartition, 0)
	for topicPartition, _ := range partitionOwnershipDecision {
		topicPartitions = append(topicPartitions, &TopicAndPartition{topicPartition.Topic, topicPartition.Partition})
	}

	offsetsFetchResponse, err := c.fetchOffsets(topicPartitions)
	if (err != nil) {
		Errorf(c, "Failed to fetch offsets during rebalance: %s", err)
		return false
	}

	currentTopicRegistry := make(map[string]map[int]*PartitionTopicInfo)

	if (c.isShuttingdown) {
		Warnf(c, "Aborting consumer '%s' rebalancing, since shutdown sequence started.", c.config.ConsumerId)
		return true
	} else {
		for _, topicPartition := range topicPartitions {
			offset := offsetsFetchResponse.Blocks[topicPartition.Topic][int32(topicPartition.Partition)].Offset
			threadId := partitionOwnershipDecision[*topicPartition]
			c.addPartitionTopicInfo(currentTopicRegistry, topicPartition, offset, threadId)
		}
	}

	if (c.reflectPartitionOwnershipDecision(partitionOwnershipDecision)) {
		c.TopicRegistry = currentTopicRegistry
		c.updateFetcher()
	} else {
		Errorf(c, "Failed to reflect partition ownership during rebalance")
		return false
	}

	return true
}

func (c *Consumer) fetchOffsets(topicPartitions []*TopicAndPartition) (*sarama.OffsetFetchResponse, error) {
	if (len(topicPartitions) == 0) {
		return &sarama.OffsetFetchResponse{}, nil
	} else {
		blocks := make(map[string]map[int32]*sarama.OffsetFetchResponseBlock)
		if (c.config.OffsetsStorage == "zookeeper") {
			for _, topicPartition := range topicPartitions {
				offset, err := GetOffsetForTopicPartition(c.zkConn, c.config.Groupid, topicPartition)
				_, exists := blocks[topicPartition.Topic]
				if (!exists) {
					blocks[topicPartition.Topic] = make(map[int32]*sarama.OffsetFetchResponseBlock)
				}
				if (err != nil) {
					return nil, err
				} else {
					blocks[topicPartition.Topic][int32(topicPartition.Partition)] = &sarama.OffsetFetchResponseBlock {
						Offset: offset,
						Metadata: "",
						Err: sarama.NoError,
					}
				}
			}
		} else {
			panic(fmt.Sprintf("Offset storage '%s' is not supported", c.config.OffsetsStorage))
		}

		return &sarama.OffsetFetchResponse{ Blocks: blocks, }, nil
	}
}

func (c *Consumer) addPartitionTopicInfo(currentTopicRegistry map[string]map[int]*PartitionTopicInfo,
	topicPartition *TopicAndPartition, offset int64,
	consumerThreadId *ConsumerThreadId) {
	partTopicInfoMap, exists := currentTopicRegistry[topicPartition.Topic]
	if (!exists) {
		partTopicInfoMap = make(map[int]*PartitionTopicInfo)
		currentTopicRegistry[topicPartition.Topic] = partTopicInfoMap
	}

	topicAndThreadId := TopicAndThreadId{topicPartition.Topic, consumerThreadId}
	var blocks *SharedBlockChannel = nil
	for topicThread, blocksChannel := range c.topicThreadIdsAndSharedChannels {
		if reflect.DeepEqual(topicAndThreadId, topicThread) {
			blocks = blocksChannel
		}
	}

	partTopicInfo := &PartitionTopicInfo{
		Topic: topicPartition.Topic,
		Partition: topicPartition.Partition,
		BlockChannel: blocks,
		ConsumedOffset: offset,
		FetchedOffset: offset,
		FetchSize: int(c.config.FetchMessageMaxBytes),
		ClientId: c.config.ConsumerId,
	}

	partTopicInfoMap[topicPartition.Partition] = partTopicInfo
	c.checkPointedZkOffsets[topicPartition] = offset
}

func (c *Consumer) reflectPartitionOwnershipDecision(partitionOwnershipDecision map[TopicAndPartition]*ConsumerThreadId) bool {
	Infof(c, "Consumer %s is trying to reflect partition ownership decision: %v\n", c.config.ConsumerId, partitionOwnershipDecision)
	successfullyOwnedPartitions := make([]*TopicAndPartition, 0)
	for topicPartition, consumerThreadId := range partitionOwnershipDecision {
		success, err := ClaimPartitionOwnership(c.zkConn, c.config.Groupid, topicPartition.Topic, topicPartition.Partition, consumerThreadId)
		if (err != nil) {
			panic(err)
		}
		if (success) {
			Debugf(c, "Consumer %s, successfully claimed partition %d for topic %s", c.config.ConsumerId, topicPartition.Partition, topicPartition.Topic)
			successfullyOwnedPartitions = append(successfullyOwnedPartitions, &topicPartition)
		} else {
			Warnf(c, "Consumer %s failed to claim partition %d for topic %s", c.config.ConsumerId, topicPartition.Partition, topicPartition.Topic)
		}
	}

	if (len(partitionOwnershipDecision) > len(successfullyOwnedPartitions)) {
		Warnf(c, "Consumer %s failed to reflect all partitions %d of %d", c.config.ConsumerId, len(successfullyOwnedPartitions), len(partitionOwnershipDecision))
		for _, topicPartition := range successfullyOwnedPartitions {
			DeletePartitionOwnership(c.zkConn, c.config.Groupid, topicPartition.Topic, topicPartition.Partition)
		}
		return false
	}

	return true
}

func (c *Consumer) releasePartitionOwnership(localTopicRegistry map[string]map[int]*PartitionTopicInfo) {
	Info(c, "Releasing partition ownership")
	for topic, partitionInfos := range localTopicRegistry {
		for partition, _ := range partitionInfos {
			err := DeletePartitionOwnership(c.zkConn, c.config.Groupid, topic, partition)
			if (err != nil) {
				if err == zk.ErrNoNode {
					Warn(c, err)
				} else {
					panic(err)
				}
			}
		}
		delete(localTopicRegistry, topic)
	}
}

func IsOffsetInvalid(offset int64) bool {
	return offset <= InvalidOffset
}

func NewChannelAndStream(config *ConsumerConfig, closeChannel chan bool) *ChannelAndStream {
	blockChannel := &SharedBlockChannel{make(chan *sarama.FetchResponseBlock, config.QueuedMaxMessages), false}
	cs := &ChannelAndStream {
		Blocks : blockChannel,
		Messages : make(chan []*Message),
		closeChannel : closeChannel,
	}

	go cs.processIncomingBlocks()
	return cs
}

func (cs *ChannelAndStream) processIncomingBlocks() {
	Debug("cs", "Started processing blocks")
	for {
		select {
		case <-cs.closeChannel: {
			return
		}
		case b := <-cs.Blocks.chunks: {
			if b != nil {
				messages := make([]*Message, 0)
				for _, message := range b.MsgSet.Messages {
					msg := &Message {
						Key : message.Msg.Key,
						Value : message.Msg.Value,
						Offset : message.Offset,
					}
					messages = append(messages, msg)
				}
				if len(messages) > 0 {
					cs.Messages <- messages
				}
			}
		}
		}
	}
}
