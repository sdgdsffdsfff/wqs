/*
Copyright 2009-2016 Weibo, Inc.

All files licensed under the Apache License, Version 2.0 (the "License");
you may not use these files except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package queue

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weibocom/wqs/config"
	"github.com/weibocom/wqs/engine/kafka"
	"github.com/weibocom/wqs/log"
	"github.com/weibocom/wqs/metrics"

	"github.com/Shopify/sarama"
	"github.com/bsm/sarama-cluster"
	"github.com/juju/errors"
)

type kafkaLogger struct {
}

func (l *kafkaLogger) Print(v ...interface{}) {
	args := []interface{}{"[sarama] "}
	args = append(args, v...)
	log.Info(args...)
}

func (l *kafkaLogger) Printf(format string, v ...interface{}) {
	log.Info("[sarama] ", fmt.Sprintf(format, v...))
}

func (l *kafkaLogger) Println(v ...interface{}) {
	args := []interface{}{"[sarama] "}
	args = append(args, v...)
	log.Info(args...)
}

func init() {
	sarama.Logger = &kafkaLogger{}
}

type queueImp struct {
	conf          *config.Config
	clusterConfig *cluster.Config
	metadata      *Metadata
	producer      *kafka.Producer
	idGenerator   *idGenerator
	consumerMap   map[string]*kafka.Consumer
	vaildName     *regexp.Regexp
	mu            sync.Mutex
	uptime        time.Time
	version       string
}

func genClusterConfig(hostname string) *cluster.Config {

	config := cluster.NewConfig()
	// Network
	config.Config.Net.KeepAlive = 30 * time.Second
	config.Config.Net.MaxOpenRequests = 20
	config.Config.Net.DialTimeout = 10 * time.Second
	config.Config.Net.ReadTimeout = 10 * time.Second
	config.Config.Net.WriteTimeout = 10 * time.Second
	// Metadata
	config.Config.Metadata.Retry.Backoff = 100 * time.Millisecond
	config.Config.Metadata.Retry.Max = 5
	config.Config.Metadata.RefreshFrequency = 1 * time.Minute
	// Producer
	config.Config.Producer.RequiredAcks = sarama.WaitForLocal
	//conf.Producer.RequiredAcks = sarama.NoResponse //this one high performance than WaitForLocal
	config.Config.Producer.Partitioner = sarama.NewRandomPartitioner
	config.Config.Producer.Flush.Frequency = time.Millisecond
	config.Config.Producer.Flush.MaxMessages = 200
	config.Config.ChannelBufferSize = 1024
	// Common
	config.Config.ClientID = fmt.Sprintf("%d..%s", os.Getpid(), hostname)
	config.Group.Heartbeat.Interval = 50 * time.Millisecond
	// Consumer
	config.Config.Consumer.Retry.Backoff = 500 * time.Millisecond
	config.Config.Consumer.Offsets.CommitInterval = 100 * time.Millisecond
	//clusterConfig.Config.Consumer.Offsets.Initial = sarama.OffsetOldest
	config.Group.Offsets.Retry.Max = 3
	config.Group.Session.Timeout = 10 * time.Second
	return config
}

func newQueue(config *config.Config, version string) (*queueImp, error) {

	hostname, err := os.Hostname()
	if err != nil {
		return nil, errors.Trace(err)
	}

	clusterConfig := genClusterConfig(hostname)
	metadata, err := NewMetadata(config, &clusterConfig.Config)
	if err != nil {
		return nil, errors.Trace(err)
	}

	srvInfo := &proxyInfo{
		Host:   hostname,
		config: config,
	}
	err = metadata.RegisterService(config.ProxyId, srvInfo.String())
	if err != nil {
		metadata.Close()
		return nil, errors.Trace(err)
	}
	producer, err := kafka.NewProducer(metadata.getLocalManager().BrokerAddrs(), &clusterConfig.Config)
	if err != nil {
		return nil, errors.Trace(err)
	}

	qs := &queueImp{
		conf:          config,
		clusterConfig: clusterConfig,
		metadata:      metadata,
		producer:      producer,
		idGenerator:   newIDGenerator(uint64(config.ProxyId)),
		vaildName:     regexp.MustCompile(`^[a-zA-Z0-9]{1,20}$`),
		consumerMap:   make(map[string]*kafka.Consumer),
		uptime:        time.Now(),
		version:       version,
	}
	return qs, nil
}

//Create a queue by name.
func (q *queueImp) Create(queue string, idcs []string) error {
	// 1. check queue name valid
	if !q.vaildName.MatchString(queue) {
		return errors.NotValidf("queue : %q", queue)
	}
	// 2. add metadata of queue
	if err := q.metadata.AddQueue(queue, idcs); err != nil {
		return errors.Trace(err)
	}
	return nil
}

//Updata queue information by name. Nothing to be update so far.
func (q *queueImp) Update(queue string) error {

	if !q.vaildName.MatchString(queue) {
		return errors.NotValidf("queue : %q", queue)
	}
	//TODO
	err := q.metadata.RefreshMetadata()
	if err != nil {
		return errors.Trace(err)
	}
	if exist := q.metadata.ExistQueue(queue); !exist {
		return errors.NotFoundf("queue : %q", queue)
	}
	//TODO 暂时没有需要更新的内容
	return nil
}

//Delete queue by name
func (q *queueImp) Delete(queue string) error {
	// 1. check queue name valid
	if !q.vaildName.MatchString(queue) {
		return errors.NotValidf("queue : %q", queue)
	}
	// 2. delete metadata of queue
	if err := q.metadata.DelQueue(queue); err != nil {
		return errors.Trace(err)
	}
	return nil
}

//Get queue information by queue name and group name
//When queue name is "" to get all queue' information.
func (q *queueImp) Lookup(queue string, group string) (queueInfos []*QueueInfo, err error) {

	err = q.metadata.RefreshMetadata()
	if err != nil {
		return nil, errors.Trace(err)
	}

	switch {
	case queue == "":
		//Get all queue's information
		queues := q.metadata.GetQueues()
		queueInfos, err = q.metadata.GetQueueInfo(queues...)
	case queue != "" && group == "":
		//Get a queue's all groups information
		queueInfos, err = q.metadata.GetQueueInfo(queue)
	default:
		//Get group's information by queue and group's name
		exist := q.metadata.ExistGroup(queue, group)
		if !exist {
			err = errors.NotFoundf("queue: %q, group : %q")
			return
		}
		queueInfos, err = q.metadata.GetQueueInfo(queue)
		if err != nil || len(queueInfos) != 1 {
			return
		}
		for _, groupConfig := range queueInfos[0].Groups {
			if groupConfig.Group == group {
				queueInfos[0].Groups = queueInfos[0].Groups[:1]
				queueInfos[0].Groups[0] = groupConfig
				return
			}
		}
		queueInfos[0].Groups = make([]*GroupConfig, 0)
	}
	return
}

func (q *queueImp) AddGroup(group string, queue string,
	write bool, read bool, url string, ips []string) error {

	if !q.vaildName.MatchString(group) || !q.vaildName.MatchString(queue) {
		return errors.NotValidf("group : %q , queue : %q", group, queue)
	}

	if err := q.metadata.AddGroupConfig(group, queue, write, read, url, ips); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (q *queueImp) UpdateGroup(group string, queue string,
	write bool, read bool, url string, ips []string) error {

	if !q.vaildName.MatchString(group) || !q.vaildName.MatchString(queue) {
		return errors.NotValidf("group : %q , queue : %q", group, queue)
	}

	if err := q.metadata.UpdateGroupConfig(group, queue, write, read, url, ips); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (q *queueImp) DeleteGroup(group string, queue string) error {

	if !q.vaildName.MatchString(group) || !q.vaildName.MatchString(queue) {
		return errors.NotValidf("group : %q , queue : %q", group, queue)
	}

	if err := q.metadata.DeleteGroupConfig(group, queue); err != nil {
		return errors.Trace(err)
	}

	return nil
}

//Get group's information
func (q *queueImp) LookupGroup(group string) ([]*GroupInfo, error) {

	groupInfos := make([]*GroupInfo, 0)
	if err := q.metadata.RefreshMetadata(); err != nil {
		return groupInfos, errors.Trace(err)
	}

	if group == "" {
		//GET all groups' information
		groupMap := q.metadata.GetGroupMap()
		for group, queues := range groupMap {

			groupInfo := GroupInfo{
				Group:  group,
				Queues: make([]*GroupConfig, 0),
			}

			for _, queue := range queues {
				groupConfig, err := q.metadata.GetGroupConfig(group, queue)
				if err != nil {
					continue
				}
				groupInfo.Queues = append(groupInfo.Queues, groupConfig)
			}
			groupInfos = append(groupInfos, &groupInfo)
		}
	} else {
		//GET one group's information
		groupMap := q.metadata.GetGroupMap()
		queues, ok := groupMap[group]
		if !ok {
			return groupInfos, errors.NotFoundf("group : %q", group)
		}

		groupInfo := GroupInfo{
			Group:  group,
			Queues: make([]*GroupConfig, 0),
		}

		for _, queue := range queues {
			groupConfig, err := q.metadata.GetGroupConfig(group, queue)
			if err != nil {
				continue
			}
			groupInfo.Queues = append(groupInfo.Queues, groupConfig)
		}
		groupInfos = append(groupInfos, &groupInfo)
	}
	return groupInfos, nil
}

func (q *queueImp) GetSingleGroup(group string, queue string) (*GroupConfig, error) {
	return q.metadata.GetGroupConfig(group, queue)
}

func (q *queueImp) SendMessage(queue string, group string, data []byte, flag uint64) (string, error) {

	start := time.Now()
	exist := q.metadata.ExistGroup(queue, group)
	if !exist {
		return "", errors.NotFoundf("queue : %q , group: %q", queue, group)
	}

	sequence := q.idGenerator.Get()
	key := fmt.Sprintf("%x:%x", sequence, flag)

	partition, offset, err := q.producer.Send(queue, []byte(key), data)
	if err != nil {
		metrics.AddCounter(metrics.CmdSetMiss, 1)
		return "", errors.Trace(err)
	}

	msgId := messageId{
		queue:     queue,
		group:     group,
		idc:       q.metadata.local,
		partition: partition,
		offset:    offset,
		sequence:  sequence,
	}
	messageID := msgId.String()
	cost := time.Now().Sub(start).Nanoseconds() / 1e6

	prefix := queue + "." + group + "." + metrics.CmdSet + "."
	metrics.AddCounter(metrics.CmdSet, 1)
	metrics.AddCounter(prefix+metrics.Ops, 1)
	metrics.AddCounter(prefix+metrics.ElapseTimeString(cost), 1)
	metrics.AddMeter(prefix+metrics.Qps, 1)
	metrics.AddCounter(metrics.BytesWriten, int64(len(data)))
	log.Debugf("send %s:%s key %s id %s cost %d", queue, group, key, messageID, cost)
	return messageID, nil
}

func (q *queueImp) RecvMessage(queue string, group string) (string, []byte, uint64, error) {
	var err error
	start := time.Now()
	exist := q.metadata.ExistGroup(queue, group)
	if !exist {
		return "", nil, 0, errors.NotFoundf("queue : %q , group: %q", queue, group)
	}

	owner := queue + "@" + group
	q.mu.Lock()
	consumer, ok := q.consumerMap[owner]
	if !ok {
		// 此处获取config跟之前ExistGroup并不是原子操作，存在并发风险
		queueConfig := q.metadata.GetQueueConfig(queue)
		brokerAddrs := q.metadata.GetBrokerAddrsByIdc(queueConfig.Idcs...)
		consumer, err = kafka.NewConsumer(brokerAddrs, q.clusterConfig, queue, group)
		if err != nil {
			q.mu.Unlock()
			log.Errorf("new kafka consumer: %v", err)
			return "", nil, 0, errors.Trace(err)
		}
		q.consumerMap[owner] = consumer
	}
	q.mu.Unlock()

	msg, idc, err := consumer.Recv()
	if err != nil {
		metrics.AddCounter(metrics.CmdGetMiss, 1)
		return "", nil, 0, err
	}

	var sequence, flag uint64
	tokens := strings.Split(string(msg.Key), ":")
	sequence, _ = strconv.ParseUint(tokens[0], 16, 64)
	if len(tokens) > 1 {
		flag, _ = strconv.ParseUint(tokens[1], 16, 32)
	}

	msgId := messageId{
		queue:     queue,
		group:     group,
		idc:       idc,
		partition: msg.Partition,
		offset:    msg.Offset,
		sequence:  sequence,
	}
	messageID := msgId.String()

	end := time.Now()
	cost := end.Sub(start).Nanoseconds() / 1e6
	delay := end.UnixNano()/1e6 - baseTime - int64((sequence>>24)&0xFFFFFFFFFF)

	prefix := queue + "." + group + "." + metrics.CmdGet + "."
	metrics.AddCounter(metrics.CmdGet, 1)
	metrics.AddCounter(prefix+metrics.Ops, 1)
	metrics.AddCounter(prefix+metrics.ElapseTimeString(cost), 1)
	metrics.AddMeter(prefix+metrics.Qps, 1)
	metrics.AddTimer(prefix+metrics.Latency, delay)
	metrics.AddCounter(metrics.BytesRead, int64(len(msg.Value)))

	log.Debugf("recv %s:%s key %s id %s cost %d delay %d", queue, group, string(msg.Key), messageID, cost, delay)
	return messageID, msg.Value, flag, nil
}

// ACK 一条消息，ACK表明该ID的消息已经被client获取到，可以从清除
func (q *queueImp) AckMessage(queue string, group string, id string) error {

	start := time.Now()
	if exist := q.metadata.ExistGroup(queue, group); !exist {
		return errors.NotFoundf("queue : %q , group: %q", queue, group)
	}

	owner := queue + "@" + group
	q.mu.Lock()
	consumer, ok := q.consumerMap[owner]
	q.mu.Unlock()
	if !ok {
		return errors.NotFoundf("group consumer")
	}

	msgId := &messageId{}
	if err := msgId.Parse(id); err != nil {
		return errors.NotValidf("message id: %q", id)
	}

	if err := consumer.Ack(msgId.idc, msgId.partition, msgId.offset); err != nil {
		return err
	}

	cost := time.Now().Sub(start).Nanoseconds() / 1e6
	prefix := queue + "." + group + "." + metrics.CmdAck + "."
	metrics.AddCounter(prefix+metrics.Ops, 1)
	metrics.AddCounter(prefix+metrics.ElapseTimeString(cost), 1)
	metrics.AddMeter(prefix+metrics.Qps, 1)
	log.Debugf("ack %s:%s key nil id %s cost %d", queue, group, id, cost)
	return nil
}

func (q *queueImp) AccumulationStatus() ([]AccumulationInfo, error) {
	accumulationInfos := make([]AccumulationInfo, 0)
	err := q.metadata.RefreshMetadata()
	if err != nil {
		return nil, errors.Trace(err)
	}

	queueMap := q.metadata.GetQueueMap()
	for queue, groups := range queueMap {
		for _, group := range groups {
			total, consumed, err := q.metadata.Accumulation(queue, group)
			if err != nil {
				return nil, err
			}
			accumulationInfos = append(accumulationInfos, AccumulationInfo{
				Group:    group,
				Queue:    queue,
				Total:    total,
				Consumed: consumed,
			})
		}
	}
	return accumulationInfos, nil
}

func (q *queueImp) GetProxys() (map[string]string, error) {
	return q.metadata.GetProxys()
}

func (q *queueImp) GetProxyConfigByID(id int) (string, error) {
	return q.metadata.GetProxyConfigByID(id)
}

func (q *queueImp) GetUpTime() int64 {
	return time.Since(q.uptime).Nanoseconds() / 1e9
}
func (q *queueImp) GetVersion() string {
	return q.version
}

// Close 只能调用一次
func (q *queueImp) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	err := q.producer.Close()
	if err != nil {
		log.Errorf("close producer err: %s", err)
	}

	for name, consumer := range q.consumerMap {
		consumer.Close()
		delete(q.consumerMap, name)
	}

	q.metadata.Close()
}
