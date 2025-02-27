//
// Copyright Strimzi authors.
// License: Apache License 2.0 (see the file LICENSE or http://apache.org/licenses/LICENSE-2.0.html).
//

// Package services defines some canary related services
package services

import (
	"sort"
	"strconv"
	"time"

	"github.com/Shopify/sarama"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/strimzi/strimzi-canary/internal/config"
)

// TopicReconcileResult contains the result of a topic reconcile
type TopicReconcileResult struct {
	// new partitions assignments across brokers
	Assignments map[int32][]int32
	// if a refresh metadata is needed
	RefreshMetadata bool
}

// TopicService defines the service for canary topic management
type TopicService struct {
	canaryConfig *config.CanaryConfig
	saramaConfig *sarama.Config
	admin        sarama.ClusterAdmin
	initialized  bool
}

var (
	topicCreationFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name:      "topic_creation_failed_total",
		Namespace: "strimzi_canary",
		Help:      "Total number of errors while creating the canary topic",
	}, []string{"topic"})

	describeClusterError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name:      "topic_describe_cluster_error_total",
		Namespace: "strimzi_canary",
		Help:      "Total number of errors while describing cluster",
	}, nil)

	describeTopicError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name:      "topic_describe_error_total",
		Namespace: "strimzi_canary",
		Help:      "Total number of errors while getting canary topic metadata",
	}, []string{"topic"})

	alterTopicAssignmentsError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name:      "topic_alter_assignments_error_total",
		Namespace: "strimzi_canary",
		Help:      "Total number of errors while altering partitions assignments for the canary topic",
	}, []string{"topic"})

	alterTopicConfigurationError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name:      "topic_alter_configuration_error_total",
		Namespace: "strimzi_canary",
		Help:      "Total number of errors while altering configuration for the canary topic",
	}, []string{"topic"})
)

// ErrExpectedClusterSize defines the error raised when the expected cluster size is not met
type ErrExpectedClusterSize struct{}

func (e *ErrExpectedClusterSize) Error() string {
	return "Current cluster size differs from the expected size"
}

// NewTopicService returns an instance of TopicService
func NewTopicService(canaryConfig *config.CanaryConfig, saramaConfig *sarama.Config) *TopicService {
	// lazy creation of the Sarama cluster admin client when reconcile for the first time or it's closed
	ts := TopicService{
		canaryConfig: canaryConfig,
		saramaConfig: saramaConfig,
		admin:        nil,
	}
	return &ts
}

// Reconcile does a reconcile on the canary topic
//
// It first checks the number of brokers and gets the topic metadata
// If topic doesn't exist it's created with a partitions assignments having
// one leader partition for each broker
//
// It topic already exists, it checks the number of partitions compared to the current brokers
// 1. if cluster scaled up, it adds partitions processing a reassignment
// 2. if cluster scaled down, it just does a reassignment.
// In case of cluster scaled down, the partitions above the number of brokers are considered orphans
// and the producer will not send messages to him
//
// If a scale up, scale down, scale up happens, it forces a leader election for having preferred leaders
func (ts *TopicService) Reconcile() (TopicReconcileResult, error) {
	result, err := ts.reconcileTopic()
	if err != nil && err.Error() == "EOF" {
		// Kafka brokers close connection to the topic service admin client not able to recover
		// Sarama issues: https://github.com/Shopify/sarama/issues/2042, https://github.com/Shopify/sarama/issues/1796
		// Workaround closing the topic service with its admin client and the reopen on next reconcile
		ts.Close()
	}
	return result, err
}

func (ts *TopicService) reconcileTopic() (TopicReconcileResult, error) {
	result := TopicReconcileResult{nil, false}

	if ts.admin == nil {
		glog.Infof("Creating Sarama cluster admin")
		admin, err := sarama.NewClusterAdmin(ts.canaryConfig.BootstrapServers, ts.saramaConfig)
		if err != nil {
			glog.Errorf("Error creating the Sarama cluster admin: %v", err)
			return result, err
		}
		ts.admin = admin
	}

	// getting brokers for assigning canary topic replicas accordingly
	// on creation or cluster scale up/down when topic already exists
	brokers, _, err := ts.admin.DescribeCluster()
	if err != nil {
		describeClusterError.With(nil).Inc()
		glog.Errorf("Error describing cluster: %v", err)
		return result, err
	}

	metadata, err := ts.admin.DescribeTopics([]string{ts.canaryConfig.Topic})
	if err != nil {
		labels := prometheus.Labels{
			"topic": ts.canaryConfig.Topic,
		}
		describeTopicError.With(labels).Inc()
		glog.Errorf("Error retrieving metadata for topic %s: %v", ts.canaryConfig.Topic, err)
		return result, err
	}
	topicMetadata := metadata[0]

	if topicMetadata.Err == sarama.ErrUnknownTopicOrPartition {

		// canary topic doesn't exist, going to create it
		glog.V(1).Infof("The canary topic %s doesn't exist", topicMetadata.Name)
		// topic is created if "dynamic" reassignment is enabled or the expected brokers are provided by the describe cluster
		if ts.isDynamicReassignmentEnabled() || ts.canaryConfig.ExpectedClusterSize == len(brokers) {

			if result.Assignments, err = ts.createTopic(brokers); err != nil {
				labels := prometheus.Labels{
					"topic": topicMetadata.Name,
				}
				topicCreationFailed.With(labels).Inc()
				glog.Errorf("Error creating topic %s: %v", topicMetadata.Name, err)
				return result, err
			}
			glog.Infof("The canary topic %s was created", topicMetadata.Name)
		} else {
			glog.Warningf("The canary topic %s wasn't created. Expected brokers %d, Actual brokers %d",
				topicMetadata.Name, ts.canaryConfig.ExpectedClusterSize, len(brokers))
			// not creating the topic and returning error to avoid starting producer/consumer
			return result, &ErrExpectedClusterSize{}
		}
	} else if topicMetadata.Err == sarama.ErrNoError {
		// canary topic already exists
		glog.V(1).Infof("The canary topic %s already exists", topicMetadata.Name)
		logTopicMetadata(topicMetadata)

		// topic exists so altering the configuration with the provided one (only at startup)
		if !ts.initialized {
			if err := ts.alterTopicConfiguration(); err != nil {
				labels := prometheus.Labels{
					"topic": topicMetadata.Name,
				}
				alterTopicConfigurationError.With(labels).Inc()
				glog.Errorf("Error altering topic configuration %s: %v", topicMetadata.Name, err)
				return result, err
			}
		}

		// topic partitions reassignment happens if "dynamic" reassignment is enabled
		// or the topic service is just starting up with the expected number of brokers
		if ts.isDynamicReassignmentEnabled() || (!ts.initialized && ts.canaryConfig.ExpectedClusterSize == len(brokers)) {

			glog.Infof("Going to reassign topic partitions if needed")
			result.RefreshMetadata = len(brokers) != len(topicMetadata.Partitions)
			if result.Assignments, err = ts.alterTopicAssignments(len(topicMetadata.Partitions), brokers); err != nil {
				labels := prometheus.Labels{
					"topic": topicMetadata.Name,
				}
				alterTopicAssignmentsError.With(labels).Inc()
				glog.Errorf("Error reassigning partitions for topic %s: %v", topicMetadata.Name, err)
				return result, err
			}
			ts.isPreferredLeaderElectionNeeded(len(brokers), topicMetadata)
			// TODO force a leader election. The feature is missing in Sarama library right now.
		} else {
			result.Assignments = ts.currentAssignments(topicMetadata)
		}
	} else {
		labels := prometheus.Labels{
			"topic": ts.canaryConfig.Topic,
		}
		describeTopicError.With(labels).Inc()
		glog.Errorf("Error retrieving metadata for topic %s: %s", ts.canaryConfig.Topic, topicMetadata.Err.Error())
		return result, topicMetadata.Err
	}

	ts.initialized = true
	return result, err
}

// Close closes the underneath Sarama admin instance
func (ts *TopicService) Close() {
	glog.Infof("Closing topic service")

	if err := ts.admin.Close(); err != nil {
		glog.Fatalf("Error closing the Sarama cluster admin: %v", err)
	}
	ts.admin = nil
	glog.Infof("Topic service closed")
}

func (ts *TopicService) alterTopicConfiguration() error {
	topicConfig := make(map[string]*string, len(ts.canaryConfig.TopicConfig))
	for index, param := range ts.canaryConfig.TopicConfig {
		p := param
		topicConfig[index] = &p
	}
	if len(topicConfig) != 0 {
		return ts.admin.AlterConfig(sarama.TopicResource, ts.canaryConfig.Topic, topicConfig, false)
	}
	return nil
}

func (ts *TopicService) createTopic(brokers []*sarama.Broker) (map[int32][]int32, error) {
	assignments, minISR := ts.requestedAssignments(0, brokers)

	v := strconv.Itoa(int(minISR))
	topicConfig := make(map[string]*string, len(ts.canaryConfig.TopicConfig))
	for index, param := range ts.canaryConfig.TopicConfig {
		p := param
		topicConfig[index] = &p
	}
	topicConfig["min.insync.replicas"] = &v

	topicDetail := sarama.TopicDetail{
		NumPartitions:     -1,
		ReplicationFactor: -1,
		ReplicaAssignment: assignments,
		ConfigEntries:     topicConfig,
	}
	err := ts.admin.CreateTopic(ts.canaryConfig.Topic, &topicDetail, false)
	return assignments, err
}

func (ts *TopicService) alterTopicAssignments(currentPartitions int, brokers []*sarama.Broker) (map[int32][]int32, error) {
	brokersNumber := len(brokers)
	assignmentsMap, _ := ts.requestedAssignments(currentPartitions, brokers)

	assignments := make([][]int32, len(assignmentsMap))
	for i := 0; i < len(assignments); i++ {
		assignments[i] = make([]int32, len(assignmentsMap[int32(i)]))
		copy(assignments[i], assignmentsMap[int32(i)])
	}

	var err error
	// less partitions than brokers (scale up)
	if currentPartitions < brokersNumber {
		// when replication factor is less than 3 because brokers are not 3 yet (see replicationFactor := min(brokersNumber, 3)),
		// it's not possible to create the new partitions directly with a replication factor higher than the current ones.
		// So first alter the assignment of current partitions with new replicas (higher replication factor)
		if err = ts.alterAssignments(assignments[:currentPartitions]); err == nil {
			// passing the assigments just for the partitions that needs to be created
			err = ts.admin.CreatePartitions(ts.canaryConfig.Topic, int32(brokersNumber), assignments[currentPartitions:], false)
		}
	} else {
		// more or equals partitions than brokers, just need reassignment
		err = ts.alterAssignments(assignments[:currentPartitions])
	}
	return assignmentsMap, err
}

func (ts *TopicService) isPreferredLeaderElectionNeeded(brokersNumber int, metadata *sarama.TopicMetadata) {
	electLeader := false
	if len(metadata.Partitions) == brokersNumber {
		for _, p := range metadata.Partitions {
			if p.ID != p.Leader {
				electLeader = true
			}
		}
	}
	glog.V(2).Infof("Elect leader = %t", electLeader)
}

func (ts *TopicService) requestedAssignments(currentPartitions int, brokers []*sarama.Broker) (map[int32][]int32, int) {
	brokersNumber := len(brokers)
	partitions := max(currentPartitions, brokersNumber)
	replicationFactor := min(brokersNumber, 3)
	minISR := max(1, replicationFactor-1)

	// partitions assignments algorithm is simpler and works effectively if brokers are ordered by ID
	// it could not be the case from a Metadata request, so sorting them first
	sort.Slice(brokers, func(i, j int) bool {
		return brokers[i].ID() < brokers[j].ID()
	})

	assignments := make(map[int32][]int32, int(partitions))
	for p := 0; p < int(partitions); p++ {
		assignments[int32(p)] = make([]int32, int(replicationFactor))
		k := p
		for r := 0; r < int(replicationFactor); r++ {
			// get brokers ID for assignment from the brokers list and not using
			// just a monotonic increasing index because there could be "hole" (a broker down)
			assignments[int32(p)][r] = brokers[int32(k%int(brokersNumber))].ID()
			k++
		}
	}
	glog.V(1).Infof("Topic %s requested partitions assignments = %v, minISR = %d", ts.canaryConfig.Topic, assignments, int(minISR))
	return assignments, int(minISR)
}

func (ts *TopicService) currentAssignments(topicMetadata *sarama.TopicMetadata) map[int32][]int32 {
	assignments := make(map[int32][]int32, len(topicMetadata.Partitions))
	for _, p := range topicMetadata.Partitions {
		assignments[p.ID] = make([]int32, len(p.Replicas))
		copy(assignments[p.ID], p.Replicas)
	}
	return assignments
}

// Alter the replica assignment for the partitions
//
// After the request for the replica assignement, it run a loop for checking if the reassignment is still ongoing
// It returns when the reassignment is done or there is an error
func (ts *TopicService) alterAssignments(assignments [][]int32) error {
	if err := ts.admin.AlterPartitionReassignments(ts.canaryConfig.Topic, assignments); err != nil {
		return err
	}

	partitions := make([]int32, 0, len(assignments))
	for partitionID := range assignments {
		partitions = append(partitions, int32(partitionID))
	}
	// loop for checking that there is no ongoing reassignments
	for {
		ongoing := false
		reassignments, err := ts.admin.ListPartitionReassignments(ts.canaryConfig.Topic, partitions)
		if err != nil {
			return nil
		}
		// on each partition of the topic shouldn't be adding or removing replicas ongoing
		for _, reassignmentStatus := range reassignments[ts.canaryConfig.Topic] {
			glog.V(1).Infof("List reassignments = %+v", reassignmentStatus)
			ongoing = ongoing || (len(reassignmentStatus.AddingReplicas) != 0 || len(reassignmentStatus.RemovingReplicas) != 0)
		}
		if !ongoing {
			break
		}
		time.Sleep(2000 * time.Millisecond)
	}
	return nil
}

// If the "dynamic" topic partitions reassignment is enabled
func (ts *TopicService) isDynamicReassignmentEnabled() bool {
	return ts.canaryConfig.ExpectedClusterSize == config.ExpectedClusterSizeDefault
}

func max(x, y int) int {
	if x < y {
		return y
	}
	return x
}

func min(x, y int) int {
	if x > y {
		return y
	}
	return x
}

func logTopicMetadata(topicMetadata *sarama.TopicMetadata) {
	// sorting partitions first, as it could not be from a Metadata request and it's better for logging
	sort.Slice(topicMetadata.Partitions, func(i, j int) bool {
		return topicMetadata.Partitions[i].ID < topicMetadata.Partitions[j].ID
	})
	glog.V(1).Infof("Metadata for %s topic", topicMetadata.Name)
	for _, p := range topicMetadata.Partitions {
		glog.V(1).Infof("\t{ID:%d Leader:%d Replicas:%v Isr:%v OfflineReplicas:%v}", p.ID, p.Leader, p.Replicas, p.Isr, p.OfflineReplicas)
	}
}
