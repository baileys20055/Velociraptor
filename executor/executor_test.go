package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"www.velocidex.com/golang/velociraptor/actions"
	actions_proto "www.velocidex.com/golang/velociraptor/actions/proto"
	"www.velocidex.com/golang/velociraptor/config"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	"www.velocidex.com/golang/velociraptor/file_store/test_utils"
	"www.velocidex.com/golang/velociraptor/responder"
	"www.velocidex.com/golang/velociraptor/utils"
	"www.velocidex.com/golang/velociraptor/vtesting"
	"www.velocidex.com/golang/velociraptor/vtesting/assert"
)

type ExecutorTestSuite struct {
	test_utils.TestSuite
}

func (self *ExecutorTestSuite) SetupTest() {
	self.TestSuite.SetupTest()

	err := self.Sm.Start(responder.StartFlowManager)
	assert.NoError(self.T(), err)

	flow_manager := responder.GetFlowManager(self.Ctx, self.ConfigObj)
	flow_manager.ResetForTests()
}

// Cancelling the executor multiple times will cause a single
// cancellation state and then ignore the rest.
func (self *ExecutorTestSuite) TestCancellation() {
	t := self.T()

	// Max time for deadlock detection.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()

	config_obj := config.GetDefaultConfig()
	executor, err := NewClientExecutor(ctx, "", config_obj)
	require.NoError(t, err)

	flow_id := fmt.Sprintf("F.XXX%d", utils.GetId())

	actions.QueryLog.Clear()

	var mu sync.Mutex
	var received_messages []*crypto_proto.VeloMessage

	// Collect outbound messages
	go func() {
		for {
			select {
			// Wait here until the executor is fully cancelled.
			case <-ctx.Done():
				return

			case message := <-executor.Outbound:
				mu.Lock()
				received_messages = append(
					received_messages, message)
				mu.Unlock()
			}
		}
	}()

	// Send the executor a flow request
	executor.Inbound <- &crypto_proto.VeloMessage{
		AuthState: crypto_proto.VeloMessage_AUTHENTICATED,
		SessionId: flow_id,
		FlowRequest: &crypto_proto.FlowRequest{
			VQLClientActions: []*actions_proto.VQLCollectorArgs{{
				Query: []*actions_proto.VQLRequest{
					{
						Name: "Query",
						VQL:  "SELECT sleep(time=1000) FROM scope()",
					},
				},
			}},
		},
	}

	// Wait until the query starts running.
	vtesting.WaitUntil(time.Second*50, self.T(), func() bool {
		return len(actions.QueryLog.Get()) > 0
	})

	cancel_message := &crypto_proto.VeloMessage{
		AuthState: crypto_proto.VeloMessage_AUTHENTICATED,
		SessionId: flow_id,
		Cancel:    &crypto_proto.Cancel{},
		RequestId: 1}

	// Send many cancel messages
	for i := 0; i < 10; i++ {
		executor.Inbound <- cancel_message
	}

	// The cancel message should generate 1 log + a status
	// message. This should only be done once, no matter how many
	// cancellations are sent.
	vtesting.WaitUntil(time.Second*5, self.T(), func() bool {
		mu.Lock()
		defer mu.Unlock()

		// Should be at least one stat message and several log messages
		return len(received_messages) >= 3 &&
			getFlowStat(received_messages) != nil
	})

	mu.Lock()
	defer mu.Unlock()

	// An error status
	stats := getFlowStat(received_messages)
	assert.Equal(self.T(), crypto_proto.VeloStatus_GENERIC_ERROR,
		stats.FlowStats.QueryStatus[0].Status)
	assert.Contains(self.T(), getLogMessages(received_messages),
		"Cancelled all inflight queries")
}

func getFlowStat(messages []*crypto_proto.VeloMessage) *crypto_proto.VeloMessage {
	for _, m := range messages {
		if m.FlowStats != nil {
			return m
		}
	}
	return nil
}

func getLogMessages(messages []*crypto_proto.VeloMessage) string {
	res := ""
	for _, m := range messages {
		if m.LogMessage != nil {
			res += m.LogMessage.Jsonl
		}
	}
	return res
}

// Cancelling the executor multiple times will cause a single
// cancellation state and then ignore the rest.
func (self *ExecutorTestSuite) TestLogMessages() {
	t := self.T()

	// Max time for deadlock detection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	config_obj := config.GetDefaultConfig()
	executor, err := NewClientExecutor(ctx, "", config_obj)
	require.NoError(t, err)

	var mu sync.Mutex
	var received_messages []*crypto_proto.VeloMessage

	// Collect outbound messages
	go func() {
		for {
			select {
			// Wait here until the executor is fully cancelled.
			case <-ctx.Done():
				return

			case message := <-executor.Outbound:
				mu.Lock()
				received_messages = append(
					received_messages, message)
				mu.Unlock()
			}
		}
	}()

	// Send two requests for the same flow in parallel these should
	// generate a bunch of log messages.
	flow_id := fmt.Sprintf("F.XXX%d", utils.GetId())
	executor.Inbound <- &crypto_proto.VeloMessage{
		AuthState: crypto_proto.VeloMessage_AUTHENTICATED,
		SessionId: flow_id,
		FlowRequest: &crypto_proto.FlowRequest{
			VQLClientActions: []*actions_proto.VQLCollectorArgs{{
				Query: []*actions_proto.VQLRequest{
					// Log 10 messages
					{
						Name: "LoggingArtifact",
						VQL:  "SELECT log(message='log %v', args=count()) FROM range(end=10)",
					},
				}}},
		},
		RequestId: 1}

	// collect the log messages and ensure they are all batched in one response.
	log_messages := []*crypto_proto.LogMessage{}

	vtesting.WaitUntil(2*time.Second, self.T(), func() bool {
		mu.Lock()
		defer mu.Unlock()

		var total_messages uint64
		log_messages = nil

		for _, msg := range received_messages {
			if msg.LogMessage != nil {
				log_messages = append(log_messages, msg.LogMessage)

				// Each log message should have its Id field the equal
				// to the next expected row.
				assert.Equal(self.T(), msg.LogMessage.Id, int64(total_messages))
				total_messages += msg.LogMessage.NumberOfRows
			}
		}

		return total_messages > 10
	})

	// Log messages should be combined into few messages.
	assert.True(self.T(), len(log_messages) <= 2, "Too many log messages")
}

func TestExecutorTestSuite(t *testing.T) {
	suite.Run(t, new(ExecutorTestSuite))
}
