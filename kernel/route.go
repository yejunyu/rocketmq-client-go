/*
Licensed to the Apache Software Foundation (ASF) under one or more
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
limitations under the License.
*/

package kernel

import (
	"encoding/json"
	"errors"
	"github.com/apache/rocketmq-client-go/remote"
	"github.com/apache/rocketmq-client-go/rlog"
	"github.com/tidwall/gjson"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	requestTimeout   = 3000
	defaultTopic     = "TBW102"
	defaultQueueNums = 4
	MasterId         = int64(0)
)

var (
	ErrTopicNotExist = errors.New("topic not exist")
)

var (
	// brokerName -> *BrokerData
	brokerAddressesMap sync.Map

	// brokerName -> map[string]int32
	brokerVersionMap sync.Map

	publishInfoMap sync.Map
	routeDataMap   sync.Map
	lockNamesrv    sync.Mutex
)

// key is topic, value is TopicPublishInfo
type TopicPublishInfo struct {
	OrderTopic          bool
	HaveTopicRouterInfo bool
	MqList              []*MessageQueue
	RouteData           *topicRouteData
	TopicQueueIndex     int32
}

func (info *TopicPublishInfo) isOK() (bIsTopicOk bool) {
	return len(info.MqList) > 0
}

func (info *TopicPublishInfo) fetchQueueIndex() int {
	length := len(info.MqList)
	if length <= 0 {
		return -1
	}
	qIndex := atomic.AddInt32(&info.TopicQueueIndex, 1)
	return int(qIndex) % length
}

func UpdateTopicRouteInfo(topic string) {
	// Todo process lock timeout
	lockNamesrv.Lock()
	defer lockNamesrv.Unlock()

	routeData, err := queryTopicRouteInfoFromServer(topic, requestTimeout)
	if err != nil {
		rlog.Warnf("query topic route from server error: %s", err)
		return
	}

	if routeData == nil {
		rlog.Warnf("queryTopicRouteInfoFromServer return nil, Topic: %s", topic)
		return
	}

	var changed bool
	oldRouteData, exist := routeDataMap.Load(topic)
	if !exist || routeData == nil {
		changed = true
	} else {
		changed = topicRouteDataIsChange(oldRouteData.(*topicRouteData), routeData)
	}

	if !changed {
		changed = isNeedUpdateTopicRouteInfo(topic)
	} else {
		rlog.Infof("the topic[%s] route info changed, old[%v] ,new[%s]", topic, oldRouteData, routeData)
	}

	if !changed {
		return
	}

	newTopicRouteData := routeData.clone()

	for _, brokerData := range newTopicRouteData.BrokerDataList {
		brokerAddressesMap.Store(brokerData.BrokerName, brokerData.BrokerAddresses)
	}

	// update publish info
	publishInfo := routeData2PublishInfo(topic, routeData)
	publishInfo.HaveTopicRouterInfo = true

	old, _ := publishInfoMap.Load(topic)
	publishInfoMap.Store(topic, publishInfoMap)
	if old != nil {
		rlog.Infof("Old TopicPublishInfo [%s] removed.", old)
	}
}

func FindBrokerAddressInPublish(brokerName string) string {
	bd, exist := brokerAddressesMap.Load(brokerName)

	if !exist {
		return ""
	}

	return bd.(*BrokerData).BrokerAddresses[MasterId]
}

func FindBrokerAddressInSubscribe(brokerName string, brokerId int64, onlyThisBroker bool) *FindBrokerResult {
	var (
		brokerAddr = ""
		slave      = false
		found      = false
	)

	addrs, exist := brokerAddressesMap.Load(brokerName)

	if exist {
		for k, v := range addrs.(map[int64]string) {
			if v != "" {
				found = true
				if k != MasterId {
					slave = true
				}
				brokerAddr = v
				break
			}
		}
	}

	var result *FindBrokerResult
	if found {
		result = &FindBrokerResult{
			BrokerAddr:    brokerAddr,
			Slave:         slave,
			BrokerVersion: findBrokerVersion(brokerName, brokerAddr),
		}
	}

	return result
}

func FetchSubscribeMessageQueues(topic string) ([]*MessageQueue, error) {
	routeData, err := queryTopicRouteInfoFromServer(topic, 3*time.Second)

	if err != nil {
		return nil, err
	}

	mqs := make([]*MessageQueue, 0)

	for _, qd := range routeData.QueueDataList {
		if queueIsReadable(qd.Perm) {
			for i := 0; i < qd.ReadQueueNums; i++ {
				mqs = append(mqs, &MessageQueue{Topic: topic, BrokerName: qd.BrokerName, QueueId: i})
			}
		}
	}
	return mqs, nil
}

func findBrokerVersion(brokerName, brokerAddr string) int {
	versions, exist := brokerVersionMap.Load(brokerName)

	if !exist {
		return 0
	}

	v, exist := versions.(map[string]int)[brokerAddr]

	if exist {
		return v
	}
	return 0
}

func queryTopicRouteInfoFromServer(topic string, timeout time.Duration) (*topicRouteData, error) {
	request := &GetRouteInfoRequest{
		Topic: topic,
	}
	rc := remote.NewRemotingCommand(ReqGetRouteInfoByTopic, request, nil)
	response, err := remote.InvokeSync(getNameServerAddress(), rc, timeout)

	if err != nil {
		return nil, err
	}

	switch response.Code {
	case ResSuccess:
		if response.Body == nil {
			return nil, errors.New(response.Remark)
		}
		routeData := &topicRouteData{}

		err = routeData.decode(string(response.Body))
		if err != nil {
			rlog.Warnf("decode topicRouteData error: %s", err)
			return nil, err
		}
		return routeData, nil
	case ResTopicNotExist:
		return nil, ErrTopicNotExist
	default:
		return nil, errors.New(response.Remark)
	}
}

func topicRouteDataIsChange(oldData *topicRouteData, newData *topicRouteData) bool {
	if oldData == nil || newData == nil {
		return true
	}
	oldDataCloned := oldData.clone()
	newDataCloned := newData.clone()

	sort.Slice(oldDataCloned.QueueDataList, func(i, j int) bool {
		return strings.Compare(oldDataCloned.QueueDataList[i].BrokerName, oldDataCloned.QueueDataList[j].BrokerName) > 0
	})
	sort.Slice(oldDataCloned.BrokerDataList, func(i, j int) bool {
		return strings.Compare(oldDataCloned.BrokerDataList[i].BrokerName, oldDataCloned.BrokerDataList[j].BrokerName) > 0
	})
	sort.Slice(newDataCloned.QueueDataList, func(i, j int) bool {
		return strings.Compare(newDataCloned.QueueDataList[i].BrokerName, newDataCloned.QueueDataList[j].BrokerName) > 0
	})
	sort.Slice(newDataCloned.BrokerDataList, func(i, j int) bool {
		return strings.Compare(newDataCloned.BrokerDataList[i].BrokerName, newDataCloned.BrokerDataList[j].BrokerName) > 0
	})

	return !oldDataCloned.equals(newDataCloned)
}

func isNeedUpdateTopicRouteInfo(topic string) bool {
	value, exist := publishInfoMap.Load(topic)

	return !exist || value.(*TopicPublishInfo).isOK()
}

func routeData2PublishInfo(topic string, data *topicRouteData) *TopicPublishInfo {
	publishInfo := &TopicPublishInfo{
		RouteData:  data,
		OrderTopic: false,
	}

	if data.OrderTopicConf != "" {
		brokers := strings.Split(data.OrderTopicConf, ";")
		for _, broker := range brokers {
			item := strings.Split(broker, ":")
			nums, _ := strconv.Atoi(item[1])
			for i := 0; i < nums; i++ {
				mq := &MessageQueue{
					Topic:      topic,
					BrokerName: item[0],
					QueueId:    i,
				}
				publishInfo.MqList = append(publishInfo.MqList, mq)
			}
		}

		publishInfo.OrderTopic = true
		return publishInfo
	}

	qds := data.QueueDataList
	sort.Slice(qds, func(i, j int) bool {
		return i-j >= 0
	})

	for _, qd := range qds {
		if !queueIsWriteable(qd.Perm) {
			continue
		}

		var bData *BrokerData
		for _, bd := range data.BrokerDataList {
			if bd.BrokerName == qd.BrokerName {
				bData = bd
				break
			}
		}

		if bData == nil || bData.BrokerAddresses[MasterId] == "" {
			continue
		}

		for i := 0; i < qd.WriteQueueNums; i++ {
			mq := &MessageQueue{
				Topic:      topic,
				BrokerName: qd.BrokerName,
				QueueId:    i,
			}
			publishInfo.MqList = append(publishInfo.MqList, mq)
		}
	}

	return publishInfo
}

func getNameServerAddress() string {
	return "127.0.0.1:9876"
}

// topicRouteData topicRouteData
type topicRouteData struct {
	OrderTopicConf string
	QueueDataList  []*QueueData  `json:"queueDatas"`
	BrokerDataList []*BrokerData `json:"brokerDatas"`
}

func (routeData *topicRouteData) decode(data string) error {
	res := gjson.Parse(data)
	json.Unmarshal([]byte(res.Get("queueDatas").String()), &routeData.QueueDataList)

	bds := res.Get("brokerDatas").Array()
	routeData.BrokerDataList = make([]*BrokerData, len(bds))
	for idx, v := range bds {
		bd := &BrokerData{
			BrokerName:      v.Get("brokerName").String(),
			Cluster:         v.Get("cluster").String(),
			BrokerAddresses: make(map[int64]string, 0),
		}
		addrs := v.Get("brokerAddrs").String()
		strs := strings.Split(addrs[1:len(addrs)-1], ",")
		if strs != nil {
			for _, str := range strs {
				i := strings.Index(str, ":")
				if i < 0 {
					continue
				}
				id, _ := strconv.ParseInt(str[0:i], 10, 64)
				bd.BrokerAddresses[id] = strings.Replace(str[i+1:], "\"", "", -1)
			}
		}
		routeData.BrokerDataList[idx] = bd
	}
	return nil
}

func (routeData *topicRouteData) clone() *topicRouteData {
	cloned := &topicRouteData{
		OrderTopicConf: routeData.OrderTopicConf,
		QueueDataList:  make([]*QueueData, len(routeData.QueueDataList)),
		BrokerDataList: make([]*BrokerData, len(routeData.BrokerDataList)),
	}

	for index, value := range routeData.QueueDataList {
		cloned.QueueDataList[index] = value
	}

	for index, value := range routeData.BrokerDataList {
		cloned.BrokerDataList[index] = value
	}

	return cloned
}

func (routeData *topicRouteData) equals(data *topicRouteData) bool {
	return false
}

// QueueData QueueData
type QueueData struct {
	BrokerName     string `json:"brokerName"`
	ReadQueueNums  int    `json:"readQueueNums"`
	WriteQueueNums int    `json:"writeQueueNums"`
	Perm           int    `json:"perm"`
	TopicSynFlag   int    `json:"topicSynFlag"`
}

// BrokerData BrokerData
type BrokerData struct {
	Cluster             string           `json:"cluster"`
	BrokerName          string           `json:"brokerName"`
	BrokerAddresses     map[int64]string `json:"brokerAddrs"`
	brokerAddressesLock sync.RWMutex
}
